package storage

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// S3 stores blobs in an S3-compatible bucket using path-style SigV4 requests.
type S3 struct {
	endpoint string
	bucket   string
	creds    credentials
	client   *http.Client
	now      func() time.Time
}

// S3Options configures NewS3.
type S3Options struct {
	Endpoint  string // e.g. https://s3.eu-central-1.amazonaws.com
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string       // defaults to us-east-1
	Client    *http.Client // defaults to http.DefaultClient
	// AllowInsecure permits HTTP endpoints, which provide no transport integrity
	// for UNSIGNED-PAYLOAD bodies.
	AllowInsecure bool
}

// NewS3 returns an S3 store for an existing bucket.
func NewS3(opts S3Options) (*S3, error) {
	if opts.Endpoint == "" || opts.Bucket == "" {
		return nil, fmt.Errorf("storage: NewS3 needs Endpoint and Bucket")
	}
	u, err := url.Parse(opts.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage: NewS3: invalid endpoint %q", opts.Endpoint)
	}
	// Validate the raw endpoint to reject empty query or fragment suffixes.
	if u.User != nil || strings.ContainsAny(opts.Endpoint, "?#") ||
		u.Path != "" || u.Hostname() == "" {
		return nil, fmt.Errorf("storage: NewS3: endpoint %q must be scheme://host[:port] only", opts.Endpoint)
	}
	if !validBucket(opts.Bucket) {
		return nil, fmt.Errorf("storage: NewS3: invalid bucket %q", opts.Bucket)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !opts.AllowInsecure {
			return nil, fmt.Errorf("storage: NewS3: http endpoint requires AllowInsecure (body integrity relies on TLS)")
		}
	default:
		return nil, fmt.Errorf("storage: NewS3: invalid endpoint scheme %q", u.Scheme)
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, fmt.Errorf("storage: NewS3 needs AccessKey and SecretKey")
	}
	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}
	base := opts.Client
	if base == nil {
		base = http.DefaultClient
	}
	// Copy the client and refuse redirects, which could replay credentials or
	// bodies to another endpoint.
	client := &http.Client{
		Transport:     base.Transport,
		Timeout:       base.Timeout,
		Jar:           base.Jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errNoRedirects },
	}
	return &S3{
		endpoint: u.Scheme + "://" + u.Host,
		bucket:   opts.Bucket,
		creds:    credentials{accessKey: opts.AccessKey, secretKey: opts.SecretKey, region: region},
		client:   client,
		now:      time.Now,
	}, nil
}

var errNoRedirects = fmt.Errorf("storage: s3 endpoint redirected; configure the correct endpoint")

// objectURL applies the same segment escaping used by SigV4.
func (s *S3) objectURL(key string) string {
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		segs[i] = uriEscape(seg)
	}
	return s.endpoint + "/" + s.bucket + "/" + strings.Join(segs, "/")
}

func (s *S3) do(ctx context.Context, method, rawurl string, body io.Reader, payloadSHA string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawurl, body)
	if err != nil {
		return nil, err
	}
	signV4(req, s.creds, payloadSHA, s.now())
	return s.client.Do(req)
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader) (retErr error) {
	if err := opCheck(ctx, "put", key); err != nil {
		return err
	}
	// S3 PutObject requires Content-Length, so unknown-length readers spool to disk.
	body, length, cleanup, err := knownLength(ctx, r)
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("storage: put %q: clean up request body: %w", key, err))
		}
	}()
	if length == 0 {
		// NoBody preserves explicit zero-length framing in net/http.
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", s.objectURL(key), body)
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	req.ContentLength = length
	signV4(req, s.creds, unsignedPayload, s.now())
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK {
		return errors.Join(
			fmt.Errorf("storage: put %q: s3 status %d", key, resp.StatusCode),
			drainFor("put", key, resp),
		)
	}
	return drainFor("put", key, resp)
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := opCheck(ctx, "open", key); err != nil {
		return nil, err
	}
	resp, err := s.do(ctx, "GET", s.objectURL(key), nil, emptyPayloadSHA)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.Join(
			fmt.Errorf("storage: open %q: %w", key, ErrNotExist),
			drainFor("open", key, resp),
		)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Join(
			fmt.Errorf("storage: open %q: s3 status %d", key, resp.StatusCode),
			drainFor("open", key, resp),
		)
	}
	return resp.Body, nil
}

func (s *S3) Stat(ctx context.Context, key string) (Info, error) {
	if err := opCheck(ctx, "stat", key); err != nil {
		return Info{}, err
	}
	resp, err := s.do(ctx, "HEAD", s.objectURL(key), nil, emptyPayloadSHA)
	if err != nil {
		return Info{}, fmt.Errorf("storage: stat %q: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return Info{}, errors.Join(
			fmt.Errorf("storage: stat %q: %w", key, ErrNotExist),
			drainFor("stat", key, resp),
		)
	}
	if resp.StatusCode != http.StatusOK {
		return Info{}, errors.Join(
			fmt.Errorf("storage: stat %q: s3 status %d", key, resp.StatusCode),
			drainFor("stat", key, resp),
		)
	}
	mod, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	info := Info{Key: key, Size: resp.ContentLength, ModTime: mod}
	if err := drainFor("stat", key, resp); err != nil {
		return Info{}, err
	}
	return info, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := opCheck(ctx, "delete", key); err != nil {
		return err
	}
	resp, err := s.do(ctx, "DELETE", s.objectURL(key), nil, emptyPayloadSHA)
	if err != nil {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return errors.Join(
			fmt.Errorf("storage: delete %q: s3 status %d", key, resp.StatusCode),
			drainFor("delete", key, resp),
		)
	}
	return drainFor("delete", key, resp)
}

type listResult struct {
	Contents []struct {
		Key          string
		Size         int64
		LastModified time.Time
	}
	IsTruncated           bool
	NextContinuationToken string
}

func (s *S3) List(ctx context.Context, prefix string) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		if err := checkPrefix(prefix); err != nil {
			yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
			return
		}
		if err := ctx.Err(); err != nil {
			yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
			return
		}
		token := ""
		for {
			// url.Values.Encode uses "+" while SigV4 signs spaces as "%20".
			q := "list-type=2"
			if token != "" {
				q = "continuation-token=" + uriEscape(token) + "&" + q
			}
			if prefix != "" {
				q += "&prefix=" + uriEscape(prefix)
			}
			resp, err := s.do(ctx, "GET", s.endpoint+"/"+s.bucket+"?"+q, nil, emptyPayloadSHA)
			if err != nil {
				yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
				return
			}
			if resp.StatusCode != http.StatusOK {
				err := errors.Join(
					fmt.Errorf("storage: list %q: s3 status %d", prefix, resp.StatusCode),
					drainFor("list", prefix, resp),
				)
				yield(Info{}, err)
				return
			}
			var lr listResult
			err = xml.NewDecoder(resp.Body).Decode(&lr)
			err = errors.Join(err, drainFor("list", prefix, resp))
			if err != nil {
				yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
				return
			}
			for _, c := range lr.Contents {
				if err := ctx.Err(); err != nil {
					yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
					return
				}
				if !yield(Info{Key: c.Key, Size: c.Size, ModTime: c.LastModified}, nil) {
					return
				}
			}
			if !lr.IsTruncated {
				return
			}
			token = lr.NextContinuationToken
		}
	}
}

func drainFor(op, key string, resp *http.Response) error {
	_, copyErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("storage: %s %q: release response: %w", op, key, err)
	}
	return nil
}

// CreateBucket provisions the bucket, treating BucketAlreadyOwnedByYou as
// success and sending LocationConstraint outside us-east-1.
func (s *S3) CreateBucket(ctx context.Context) error {
	var body io.Reader
	payloadSHA := emptyPayloadSHA
	if s.creds.region != "us-east-1" {
		xmlBody := `<CreateBucketConfiguration><LocationConstraint>` +
			s.creds.region + `</LocationConstraint></CreateBucketConfiguration>`
		body = strings.NewReader(xmlBody)
		payloadSHA = hexSHA256([]byte(xmlBody))
	}
	resp, err := s.do(ctx, "PUT", s.endpoint+"/"+s.bucket, body, payloadSHA)
	if err != nil {
		return fmt.Errorf("storage: create bucket: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return drainFor("create bucket", s.bucket, resp)
	}
	if resp.StatusCode == http.StatusConflict {
		var e struct{ Code string }
		decodeErr := xml.NewDecoder(resp.Body).Decode(&e)
		drainErr := drainFor("create bucket", s.bucket, resp)
		if decodeErr == nil && e.Code == "BucketAlreadyOwnedByYou" {
			return drainErr
		}
		return errors.Join(fmt.Errorf("storage: create bucket: conflict %s", s.bucket), drainErr)
	}
	return errors.Join(
		fmt.Errorf("storage: create bucket: s3 status %d", resp.StatusCode),
		drainFor("create bucket", s.bucket, resp),
	)
}

// validBucket applies the portable subset of S3 bucket naming rules.
func validBucket(b string) bool {
	if len(b) < 3 || len(b) > 63 {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if (i == 0 || i == len(b)-1) && !alnum {
			return false
		}
		if !alnum && c != '-' && c != '.' {
			return false
		}
		if c == '.' && i > 0 && b[i-1] == '.' {
			return false
		}
	}
	return !isIPv4Shaped(b)
}

// AWS rejects bucket names that are valid IPv4 addresses.
func isIPv4Shaped(b string) bool {
	parts := strings.Split(b, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		n := 0
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
			n = n*10 + int(p[i]-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

// knownLength returns a non-closing body, length, and cleanup function,
// spooling unknown-length readers.
func knownLength(ctx context.Context, r io.Reader) (io.Reader, int64, func() error, error) {
	none := func() error { return nil }
	switch v := r.(type) {
	case *bytes.Reader:
		return noClose{r}, int64(v.Len()), none, nil
	case *strings.Reader:
		return noClose{r}, int64(v.Len()), none, nil
	case *bytes.Buffer:
		return noClose{r}, int64(v.Len()), none, nil
	case *os.File:
		if fi, err := v.Stat(); err == nil && fi.Mode().IsRegular() {
			if pos, err := v.Seek(0, io.SeekCurrent); err == nil {
				return noClose{r}, max(fi.Size()-pos, 0), none, nil
			}
		}
	}
	spool, err := os.CreateTemp("", "storage-s3-put-*")
	if err != nil {
		return nil, 0, none, err
	}
	cleanup := func() error {
		return errors.Join(spool.Close(), os.Remove(spool.Name()))
	}
	if err := copyChunks(ctx, spool, r); err != nil {
		return nil, 0, none, errors.Join(err, cleanup())
	}
	length, err := spool.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, 0, none, errors.Join(err, cleanup())
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, 0, none, errors.Join(err, cleanup())
	}
	return noClose{spool}, length, cleanup, nil
}

type noClose struct{ r io.Reader }

func (n noClose) Read(p []byte) (int, error) { return n.r.Read(p) }
