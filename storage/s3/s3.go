// Package s3 implements the storage backend for S3-compatible endpoints. It is
// separate from storage so that only applications using it link the signer.
package s3

import (
	"bytes"
	"context"
	"encoding/hex"
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

	"github.com/gofabrik/fabrik/storage"
)

// Store stores blobs in an S3-compatible bucket using path-style SigV4 requests.
type Store struct {
	endpoint string
	bucket   string
	creds    credentials
	client   *http.Client
	now      func() time.Time
}

// Options configures New.
type Options struct {
	Endpoint  string // e.g. https://s3.eu-central-1.amazonaws.com
	Bucket    string
	AccessKey string
	SecretKey string
	// SessionToken is the optional STS token for temporary credentials; when
	// set it is sent and signed.
	SessionToken string
	Region       string // defaults to us-east-1
	// Client supplies Transport, Timeout, and Jar; the store copies those
	// and always refuses redirects. Defaults to http.DefaultClient.
	Client *http.Client
	// AllowInsecure permits HTTP endpoints, which provide no transport integrity
	// for UNSIGNED-PAYLOAD bodies.
	AllowInsecure bool
}

// New returns an S3 store for an existing bucket.
func New(opts Options) (*Store, error) {
	if opts.Endpoint == "" || opts.Bucket == "" {
		return nil, fmt.Errorf("storage: New needs Endpoint and Bucket")
	}
	u, err := url.Parse(opts.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("storage: New: invalid endpoint %q", opts.Endpoint)
	}
	// Validate the raw endpoint to reject empty query or fragment suffixes.
	if u.User != nil || strings.ContainsAny(opts.Endpoint, "?#") ||
		u.Path != "" || u.Hostname() == "" {
		return nil, fmt.Errorf("storage: New: endpoint %q must be scheme://host[:port] only", opts.Endpoint)
	}
	if !validBucket(opts.Bucket) {
		return nil, fmt.Errorf("storage: New: invalid bucket %q", opts.Bucket)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !opts.AllowInsecure {
			return nil, fmt.Errorf("storage: New: http endpoint requires AllowInsecure (body integrity relies on TLS)")
		}
	default:
		return nil, fmt.Errorf("storage: New: invalid endpoint scheme %q", u.Scheme)
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, fmt.Errorf("storage: New needs AccessKey and SecretKey")
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
	return &Store{
		endpoint: u.Scheme + "://" + u.Host,
		bucket:   opts.Bucket,
		creds: credentials{
			accessKey:    opts.AccessKey,
			secretKey:    opts.SecretKey,
			sessionToken: opts.SessionToken,
			region:       region,
		},
		client: client,
		now:    time.Now,
	}, nil
}

var errNoRedirects = fmt.Errorf("storage: s3 endpoint redirected; configure the correct endpoint")

// uriEscape applies S3's RFC 3986 escaping to a single path segment or query
// component.
func uriEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// objectURL applies the same segment escaping used by SigV4.
func (s *Store) objectURL(key string) string {
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		segs[i] = uriEscape(seg)
	}
	return s.endpoint + "/" + s.bucket + "/" + strings.Join(segs, "/")
}

func (s *Store) do(ctx context.Context, method, rawurl string, body io.Reader, payloadSHA string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawurl, body)
	if err != nil {
		return nil, err
	}
	if err := signS3(req, s.creds, payloadSHA, s.now()); err != nil {
		return nil, err
	}
	return s.client.Do(req)
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader) (retErr error) {
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
	if err := signS3(req, s.creds, unsignedPayload, s.now()); err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
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

func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := opCheck(ctx, "open", key); err != nil {
		return nil, err
	}
	resp, err := s.do(ctx, "GET", s.objectURL(key), nil, emptyPayloadSHA)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.Join(
			fmt.Errorf("storage: open %q: %w", key, storage.ErrNotExist),
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

func (s *Store) Stat(ctx context.Context, key string) (storage.Info, error) {
	if err := opCheck(ctx, "stat", key); err != nil {
		return storage.Info{}, err
	}
	resp, err := s.do(ctx, "HEAD", s.objectURL(key), nil, emptyPayloadSHA)
	if err != nil {
		return storage.Info{}, fmt.Errorf("storage: stat %q: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return storage.Info{}, errors.Join(
			fmt.Errorf("storage: stat %q: %w", key, storage.ErrNotExist),
			drainFor("stat", key, resp),
		)
	}
	if resp.StatusCode != http.StatusOK {
		return storage.Info{}, errors.Join(
			fmt.Errorf("storage: stat %q: s3 status %d", key, resp.StatusCode),
			drainFor("stat", key, resp),
		)
	}
	mod, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	info := storage.Info{Key: key, Size: resp.ContentLength, ModTime: mod}
	if err := drainFor("stat", key, resp); err != nil {
		return storage.Info{}, err
	}
	return info, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
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

func (s *Store) List(ctx context.Context, prefix string) iter.Seq2[storage.Info, error] {
	return func(yield func(storage.Info, error) bool) {
		if err := checkPrefix(prefix); err != nil {
			yield(storage.Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
			return
		}
		if err := ctx.Err(); err != nil {
			yield(storage.Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
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
				yield(storage.Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
				return
			}
			if resp.StatusCode != http.StatusOK {
				err := errors.Join(
					fmt.Errorf("storage: list %q: s3 status %d", prefix, resp.StatusCode),
					drainFor("list", prefix, resp),
				)
				yield(storage.Info{}, err)
				return
			}
			var lr listResult
			err = xml.NewDecoder(resp.Body).Decode(&lr)
			err = errors.Join(err, drainFor("list", prefix, resp))
			if err != nil {
				yield(storage.Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
				return
			}
			for _, c := range lr.Contents {
				if err := ctx.Err(); err != nil {
					yield(storage.Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
					return
				}
				if !yield(storage.Info{Key: c.Key, Size: c.Size, ModTime: c.LastModified}, nil) {
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
func (s *Store) CreateBucket(ctx context.Context) error {
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
	if err := spoolCopy(ctx, spool, r); err != nil {
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

// spoolCopy checks ctx between reads.
func spoolCopy(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type noClose struct{ r io.Reader }

func (n noClose) Read(p []byte) (int, error) { return n.r.Read(p) }

func opCheck(ctx context.Context, op, key string) error {
	if err := storage.CheckKey(key); err != nil {
		return fmt.Errorf("storage: %s %q: %w", op, key, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: %s %q: %w", op, key, err)
	}
	return nil
}

func checkPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return storage.CheckKey(strings.TrimSuffix(prefix, "/"))
}
