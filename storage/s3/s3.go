// Package s3 implements the storage backend for S3-compatible endpoints. It is
// separate from storage so that only applications using it link the signer.
package s3

import (
	"bufio"
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
	endpoint     string
	bucket       string
	creds        credentials
	client       *http.Client
	now          func() time.Time
	opTimeout    time.Duration
	drainLimit   int64
	maxListPages int
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
	// OperationTimeout bounds one operation end to end: the request, releasing
	// the response, and decoding it. Zero means 30s, negative is a construction
	// error, and there is no unlimited value. List applies it to each page
	// exchange. Put's budget encloses the whole upload, so callers sending
	// large objects raise it.
	OperationTimeout time.Duration
	// DrainLimit caps the bytes read while releasing a response body. Zero
	// means 64 KiB, negative is a construction error.
	DrainLimit int64
	// MaxListPages caps how many pages one List fetches before it gives up on a
	// bucket that never stops paginating. Zero means 1,000, negative is a
	// construction error. A List blocks at most MaxListPages*OperationTimeout,
	// since the budget bounds each page exchange.
	MaxListPages int
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
	if opts.OperationTimeout < 0 {
		return nil, fmt.Errorf("storage: New: OperationTimeout %v must be zero or positive", opts.OperationTimeout)
	}
	if opts.DrainLimit < 0 {
		return nil, fmt.Errorf("storage: New: DrainLimit %d must be zero or positive", opts.DrainLimit)
	}
	if opts.MaxListPages < 0 {
		return nil, fmt.Errorf("storage: New: MaxListPages %d must be zero or positive", opts.MaxListPages)
	}
	opTimeout := opts.OperationTimeout
	if opTimeout == 0 {
		opTimeout = defaultOperationTimeout
	}
	drainLimit := opts.DrainLimit
	if drainLimit == 0 {
		drainLimit = defaultDrainLimit
	}
	maxListPages := opts.MaxListPages
	if maxListPages == 0 {
		maxListPages = defaultMaxListPages
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
		client:       client,
		now:          time.Now,
		opTimeout:    opTimeout,
		drainLimit:   drainLimit,
		maxListPages: maxListPages,
	}, nil
}

const (
	defaultOperationTimeout = 30 * time.Second
	defaultDrainLimit       = 64 << 10
	defaultMaxListPages     = 1000
)

// maxListEntries is the object count ListObjectsV2 returns in one page; a page
// claiming more is malformed or hostile and is rejected before the surplus is
// retained.
const maxListEntries = 1000

// maxListPageBytes caps the wire size the list decoder will read from one page.
// A page holds at most 1,000 objects and a valid 1,024-byte key XML-escapes to
// at most 5,120 bytes, so 1,000 keys cost about 5 MiB, plus each entry's Size,
// LastModified, ETag, and StorageClass tags. 8 MiB covers a maximal legitimate
// page with slack; a page that runs past it comes from a misbehaving endpoint.
const maxListPageBytes = 8 << 20

// listPageBufSize is the decoder's read-ahead granularity over the page body.
const listPageBufSize = 4096

var errNoRedirects = fmt.Errorf("storage: s3 endpoint redirected; configure the correct endpoint")

// errBudgetExceeded reports the operation budget rather than the caller's
// context, and wraps context.DeadlineExceeded so callers classify it as one.
var errBudgetExceeded = fmt.Errorf("operation budget exceeded: %w", context.DeadlineExceeded)

// budget derives the deadline every operation runs under, covering the
// request, the drain, and the decode.
func (s *Store) budget(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.opTimeout)
}

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
	ctx, cancel := s.budget(ctx)
	defer cancel()
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
			s.drainFor("put", key, resp),
		)
	}
	return s.drainFor("put", key, resp)
}

func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := opCheck(ctx, "open", key); err != nil {
		return nil, err
	}
	// The budget covers the handshake and the error paths; the returned body
	// then answers to the caller's context and to its own Close, so a download
	// slower than the budget is not cut off.
	opCtx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(s.opTimeout, func() { cancel(errBudgetExceeded) })
	resp, err := s.do(opCtx, "GET", s.objectURL(key), nil, emptyPayloadSHA)
	if err != nil {
		timer.Stop()
		// The transport reports both budget expiry and caller cancellation as
		// a canceled context, so the cause tells them apart.
		if cause := context.Cause(opCtx); errors.Is(cause, errBudgetExceeded) {
			err = cause
		}
		cancel(nil)
		return nil, fmt.Errorf("storage: open %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK {
		// The error paths stay inside the budget, so their drain cannot stall.
		status := fmt.Errorf("storage: open %q: s3 status %d", key, resp.StatusCode)
		if resp.StatusCode == http.StatusNotFound {
			status = fmt.Errorf("storage: open %q: %w", key, storage.ErrNotExist)
		}
		drainErr := s.drainFor("open", key, resp)
		timer.Stop()
		cancel(nil)
		return nil, errors.Join(status, drainErr)
	}
	// A timer that cannot be stopped has already fired, so the response raced
	// the budget and lost.
	if !timer.Stop() {
		drainErr := s.drainFor("open", key, resp)
		cancel(nil)
		return nil, errors.Join(fmt.Errorf("storage: open %q: %w", key, errBudgetExceeded), drainErr)
	}
	return detachedBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

// detachedBody releases the operation context when the caller is done reading.
type detachedBody struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func (b detachedBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel(nil)
	return err
}

func (s *Store) Stat(ctx context.Context, key string) (storage.Info, error) {
	if err := opCheck(ctx, "stat", key); err != nil {
		return storage.Info{}, err
	}
	ctx, cancel := s.budget(ctx)
	defer cancel()
	resp, err := s.do(ctx, "HEAD", s.objectURL(key), nil, emptyPayloadSHA)
	if err != nil {
		return storage.Info{}, fmt.Errorf("storage: stat %q: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return storage.Info{}, errors.Join(
			fmt.Errorf("storage: stat %q: %w", key, storage.ErrNotExist),
			s.drainFor("stat", key, resp),
		)
	}
	if resp.StatusCode != http.StatusOK {
		return storage.Info{}, errors.Join(
			fmt.Errorf("storage: stat %q: s3 status %d", key, resp.StatusCode),
			s.drainFor("stat", key, resp),
		)
	}
	mod, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	info := storage.Info{Key: key, Size: resp.ContentLength, ModTime: mod}
	if err := s.drainFor("stat", key, resp); err != nil {
		return storage.Info{}, err
	}
	return info, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if err := opCheck(ctx, "delete", key); err != nil {
		return err
	}
	ctx, cancel := s.budget(ctx)
	defer cancel()
	resp, err := s.do(ctx, "DELETE", s.objectURL(key), nil, emptyPayloadSHA)
	if err != nil {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return errors.Join(
			fmt.Errorf("storage: delete %q: s3 status %d", key, resp.StatusCode),
			s.drainFor("delete", key, resp),
		)
	}
	return s.drainFor("delete", key, resp)
}

func (s *Store) List(ctx context.Context, prefix string) iter.Seq2[storage.Info, error] {
	return func(yield func(storage.Info, error) bool) {
		fail := func(err error) {
			yield(storage.Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
		}
		if err := checkPrefix(prefix); err != nil {
			fail(err)
			return
		}
		if err := ctx.Err(); err != nil {
			fail(err)
			return
		}
		token := ""
		seen := map[string]bool{}
		pages := 0
		for {
			pages++
			// url.Values.Encode uses "+" while SigV4 signs spaces as "%20".
			q := "list-type=2"
			if token != "" {
				q = "continuation-token=" + uriEscape(token) + "&" + q
			}
			if prefix != "" {
				q += "&prefix=" + uriEscape(prefix)
			}
			// The budget bounds one page exchange, not the whole listing.
			pageCtx, cancel := s.budget(ctx)
			resp, err := s.do(pageCtx, "GET", s.endpoint+"/"+s.bucket+"?"+q, nil, emptyPayloadSHA)
			if err != nil {
				cancel()
				fail(err)
				return
			}
			if resp.StatusCode != http.StatusOK {
				err := errors.Join(
					fmt.Errorf("storage: list %q: s3 status %d", prefix, resp.StatusCode),
					s.drainFor("list", prefix, resp),
				)
				cancel()
				yield(storage.Info{}, err)
				return
			}
			entries, truncated, next, decodeErr := s.listPage(resp.Body)
			// The decoder reads only its own page, so the drain releases whatever
			// the endpoint sent past it, up to DrainLimit. Both finish before any
			// entry is yielded, so the page budget cannot race the caller.
			drainErr := s.drainFor("list", prefix, resp)
			cancel()
			for _, info := range entries {
				if err := ctx.Err(); err != nil {
					fail(err)
					return
				}
				if !yield(info, nil) {
					return
				}
			}
			if err := errors.Join(decodeErr, drainErr); err != nil {
				fail(err)
				return
			}
			if !truncated {
				return
			}
			if next == "" {
				fail(errors.New("truncated response has no continuation token"))
				return
			}
			if seen[next] {
				fail(fmt.Errorf("continuation token %q repeated", next))
				return
			}
			if pages >= s.maxListPages {
				fail(fmt.Errorf("listing exceeded %d pages", s.maxListPages))
				return
			}
			seen[next] = true
			token = next
		}
	}
}

// listPage decodes one ListObjectsV2 page into a bounded slice so the caller
// releases the body and its page budget before yielding any entry, keeping
// caller processing time off the budget. It reads no further than
// maxListPageBytes and rejects a page carrying more than maxListEntries before
// the surplus entry is decoded; entries decoded before an error are still
// returned. It reports whether the page is truncated, its continuation token,
// and any decode error.
func (s *Store) listPage(body io.Reader) (entries []storage.Info, truncated bool, next string, err error) {
	// A dedicated buffer keeps the read-ahead granularity ours: bytes it pulls
	// past the document were already read off the wire and are dropped, and the
	// release then reads up to DrainLimit more from the body itself.
	d := xml.NewDecoder(bufio.NewReaderSize(io.LimitReader(body, maxListPageBytes), listPageBufSize))
	depth := 0
	sawRoot := false
	for {
		tok, terr := d.Token()
		if terr == io.EOF {
			if !sawRoot {
				return entries, truncated, next, fmt.Errorf("empty list response")
			}
			return entries, truncated, next, nil
		}
		if terr != nil {
			return entries, truncated, next, terr
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if !sawRoot {
				if t.Name.Local != "ListBucketResult" {
					return entries, truncated, next, fmt.Errorf("unexpected root element %q", t.Name.Local)
				}
				sawRoot = true
				depth++
				continue
			}
			switch t.Name.Local {
			case "Contents":
				if len(entries) == maxListEntries {
					return entries, truncated, next, fmt.Errorf("page returned more than %d entries", maxListEntries)
				}
				var c struct {
					Key          string
					Size         int64
					LastModified time.Time
				}
				if derr := d.DecodeElement(&c, &t); derr != nil {
					return entries, truncated, next, derr
				}
				entries = append(entries, storage.Info{Key: c.Key, Size: c.Size, ModTime: c.LastModified})
			case "IsTruncated":
				if derr := d.DecodeElement(&truncated, &t); derr != nil {
					return entries, truncated, next, derr
				}
			case "NextContinuationToken":
				if derr := d.DecodeElement(&next, &t); derr != nil {
					return entries, truncated, next, derr
				}
			default:
				depth++
			}
		case xml.EndElement:
			// The root closing tag returns depth to zero; stopping there leaves any
			// trailing bytes to the drain instead of decoding into them.
			depth--
			if depth == 0 {
				return entries, truncated, next, nil
			}
		}
	}
}

// drainFor releases a response body, reading no more than DrainLimit. What is
// left unread costs the connection, which net/http then tears down instead of
// reusing.
func (s *Store) drainFor(op, key string, resp *http.Response) error {
	_, copyErr := io.Copy(io.Discard, io.LimitReader(resp.Body, s.drainLimit))
	closeErr := resp.Body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("storage: %s %q: release response: %w", op, key, err)
	}
	return nil
}

// CreateBucket provisions the bucket, treating BucketAlreadyOwnedByYou as
// success and sending LocationConstraint outside us-east-1.
func (s *Store) CreateBucket(ctx context.Context) error {
	ctx, cancel := s.budget(ctx)
	defer cancel()
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
		return s.drainFor("create bucket", s.bucket, resp)
	}
	if resp.StatusCode == http.StatusConflict {
		var e struct{ Code string }
		decodeErr := xml.NewDecoder(io.LimitReader(resp.Body, s.drainLimit)).Decode(&e)
		drainErr := s.drainFor("create bucket", s.bucket, resp)
		if decodeErr == nil && e.Code == "BucketAlreadyOwnedByYou" {
			return drainErr
		}
		return errors.Join(fmt.Errorf("storage: create bucket: conflict %s", s.bucket), drainErr)
	}
	return errors.Join(
		fmt.Errorf("storage: create bucket: s3 status %d", resp.StatusCode),
		s.drainFor("create bucket", s.bucket, resp),
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
