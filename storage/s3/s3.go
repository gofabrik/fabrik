// Package s3 implements the storage backend for S3-compatible endpoints. It is
// separate from storage so that only applications using it link the signer.
package s3

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofabrik/fabrik/storage"
)

// Store stores blobs in an S3-compatible bucket using path-style SigV4 requests.
type Store struct {
	endpoint      string
	bucket        string
	creds         credentials
	client        *http.Client
	now           func() time.Time
	opTimeout     time.Duration
	drainLimit    int64
	maxListPages  int
	spoolDir      string
	maxSpoolBytes int64
}

// Options configures New.
type Options struct {
	Endpoint  string // e.g. https://s3.eu-central-1.amazonaws.com
	Bucket    string
	AccessKey string
	SecretKey string
	// SessionToken is the optional signed STS token for temporary credentials.
	SessionToken string
	Region       string // defaults to us-east-1
	// Client supplies Transport, Timeout, and Jar. New copies these fields and
	// disables redirects. Nil uses http.DefaultClient.
	Client *http.Client
	// AllowInsecure permits HTTP endpoints, which provide no transport integrity
	// for UNSIGNED-PAYLOAD bodies.
	AllowInsecure bool
	// OperationTimeout bounds a request through response release and decoding,
	// and bounds the entire Put upload. List applies it separately to each page.
	// Zero means 30s, negative is invalid, and no value disables the timeout.
	OperationTimeout time.Duration
	// DrainLimit caps the bytes read while releasing a response body. Zero
	// means 64 KiB; negative is invalid.
	DrainLimit int64
	// MaxListPages caps pages per List and therefore bounds its total time by
	// MaxListPages*OperationTimeout. Zero means 1,000; negative is invalid.
	MaxListPages int
	// SpoolDir stores temporary files for unknown-length uploads. Empty uses
	// os.TempDir(); a configured directory must already exist.
	SpoolDir string
	// MaxSpoolBytes caps disk use for one unknown-length upload. Exceeding it
	// returns storage.ErrTooLarge before sending a request. Known-length readers
	// bypass the cap. Zero means 1 GiB, negative is invalid, and no value disables
	// the limit.
	MaxSpoolBytes int64
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
	// url.Parse accepts empty query and fragment suffixes, which endpoints forbid.
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
	if opts.MaxSpoolBytes < 0 {
		return nil, fmt.Errorf("storage: New: MaxSpoolBytes %d must be zero or positive", opts.MaxSpoolBytes)
	}
	if opts.SpoolDir != "" {
		fi, err := os.Stat(opts.SpoolDir)
		if err != nil {
			return nil, fmt.Errorf("storage: New: SpoolDir %q: %w", opts.SpoolDir, err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("storage: New: SpoolDir %q is not a directory", opts.SpoolDir)
		}
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
	maxSpoolBytes := opts.MaxSpoolBytes
	if maxSpoolBytes == 0 {
		maxSpoolBytes = defaultMaxSpoolBytes
	}
	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}
	base := opts.Client
	if base == nil {
		base = http.DefaultClient
	}
	// Redirects could replay signed credentials or bodies to another endpoint.
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
		client:        client,
		now:           time.Now,
		opTimeout:     opTimeout,
		drainLimit:    drainLimit,
		maxListPages:  maxListPages,
		spoolDir:      opts.SpoolDir,
		maxSpoolBytes: maxSpoolBytes,
	}, nil
}

const (
	defaultOperationTimeout = 30 * time.Second
	defaultDrainLimit       = 64 << 10
	defaultMaxListPages     = 1000
	defaultMaxSpoolBytes    = 1 << 30
)

// S3 limits each ListObjectsV2 page to 1,000 objects.
const maxListEntries = 1000

// A page holds at most 1,000 objects and a valid 1,024-byte key XML-escapes to
// at most 5,120 bytes, so keys cost about 5 MiB; 8 MiB leaves room for the
// remaining metadata.
const maxListPageBytes = 8 << 20

const listPageBufSize = 4096

// Error decoding uses a protocol-sized cap independent of connection draining.
const maxErrorDocBytes = 16 << 10

// Legitimate listings nest at most four levels; the higher cap bounds parser state.
const maxListXMLDepth = 16

// S3 tags and text runs are only a few kilobytes; larger tokens are malformed.
const maxListTokenBytes = 64 << 10

// Legitimate listings use only the root xmlns attribute.
const maxListElementAttrs = 8

var errNoRedirects = fmt.Errorf("storage: s3 endpoint redirected; configure the correct endpoint")

// errBudgetExceeded reports the operation budget rather than the caller's
// context, and wraps context.DeadlineExceeded so callers classify it as one.
var errBudgetExceeded = fmt.Errorf("operation budget exceeded: %w", context.DeadlineExceeded)

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
	body, length, cleanup, err := s.knownLength(ctx, r)
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
	// The internal budget covers the handshake and errors, not reading a successful body.
	opCtx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(s.opTimeout, func() { cancel(errBudgetExceeded) })
	resp, err := s.do(opCtx, "GET", s.objectURL(key), nil, emptyPayloadSHA)
	if err != nil {
		timer.Stop()
		// The cancellation cause distinguishes the internal budget from the caller.
		if cause := context.Cause(opCtx); errors.Is(cause, errBudgetExceeded) {
			err = cause
		}
		cancel(nil)
		return nil, fmt.Errorf("storage: open %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK {
		// Error response draining remains inside the operation budget.
		status := fmt.Errorf("storage: open %q: s3 status %d", key, resp.StatusCode)
		if resp.StatusCode == http.StatusNotFound {
			status = fmt.Errorf("storage: open %q: %w", key, storage.ErrNotExist)
		}
		drainErr := s.drainFor("open", key, resp)
		timer.Stop()
		cancel(nil)
		return nil, errors.Join(status, drainErr)
	}
	// A fired timer makes the response late even if the transport returned it.
	if !timer.Stop() {
		drainErr := s.drainFor("open", key, resp)
		cancel(nil)
		return nil, errors.Join(fmt.Errorf("storage: open %q: %w", key, errBudgetExceeded), drainErr)
	}
	return detachedBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

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
		// Tokens are retained as fixed-size digests so a hostile endpoint cannot
		// grow the seen set by the size of its tokens across a long listing.
		seen := map[[sha256.Size]byte]bool{}
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
			// Validate and release the page within its budget before yielding entries.
			drainErr := s.drainFor("list", prefix, resp)
			cancel()
			if err := errors.Join(decodeErr, drainErr); err != nil {
				fail(err)
				return
			}
			if truncated {
				if next == "" {
					fail(errors.New("truncated response has no continuation token"))
					return
				}
				if seen[sha256.Sum256([]byte(next))] {
					fail(errors.New("endpoint repeated a continuation token"))
					return
				}
				if pages >= s.maxListPages {
					fail(fmt.Errorf("listing exceeded %d pages", s.maxListPages))
					return
				}
			}
			for _, info := range entries {
				if err := ctx.Err(); err != nil {
					fail(err)
					return
				}
				if !yield(info, nil) {
					return
				}
			}
			if !truncated {
				return
			}
			seen[sha256.Sum256([]byte(next))] = true
			token = next
		}
	}
}

// listPage buffers a bounded ListObjectsV2 page for validation before yielding.
// Entries returned with an error are incomplete and must be discarded.
func (s *Store) listPage(body io.Reader) (entries []storage.Info, truncated bool, next string, err error) {
	// RawToken avoids namespace state. The fixed buffer bounds read-ahead, while
	// spanReader bounds individual tokens including that read-ahead.
	span := &spanReader{r: io.LimitReader(body, maxListPageBytes), allow: maxListTokenBytes}
	d := xml.NewDecoder(bufio.NewReaderSize(span, listPageBufSize))
	var (
		sawRoot    bool
		depth      int
		open       [maxListXMLDepth]xml.Name
		inContents bool
		entry      storage.Info
		field      string
		text       strings.Builder
	)
	for {
		tok, terr := d.RawToken()
		span.mark()
		if terr == io.EOF {
			if !sawRoot {
				return entries, truncated, next, fmt.Errorf("empty list response")
			}
			if depth != 0 {
				return entries, truncated, next, fmt.Errorf("response ended with %q unclosed", open[depth-1].Local)
			}
			return entries, truncated, next, nil
		}
		if terr != nil {
			if errors.Is(terr, errTokenTooLong) {
				return entries, truncated, next, errTokenTooLong
			}
			return entries, truncated, next, terr
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(t.Attr) > maxListElementAttrs {
				return entries, truncated, next, fmt.Errorf("element %q carries more than %d attributes", t.Name.Local, maxListElementAttrs)
			}
			if depth == maxListXMLDepth {
				return entries, truncated, next, fmt.Errorf("response nests deeper than %d elements", maxListXMLDepth)
			}
			open[depth] = t.Name
			depth++
			switch {
			case !sawRoot:
				if t.Name.Local != "ListBucketResult" {
					return entries, truncated, next, fmt.Errorf("unexpected root element %q", t.Name.Local)
				}
				sawRoot = true
			case field != "":
				return entries, truncated, next, fmt.Errorf("unexpected element %q inside %s", t.Name.Local, field)
			case depth == 2 && t.Name.Local == "Contents":
				if len(entries) == maxListEntries {
					return entries, truncated, next, fmt.Errorf("page returned more than %d entries", maxListEntries)
				}
				inContents = true
				entry = storage.Info{}
			case inContents && depth == 3 && (t.Name.Local == "Key" || t.Name.Local == "Size" || t.Name.Local == "LastModified"),
				!inContents && depth == 2 && (t.Name.Local == "IsTruncated" || t.Name.Local == "NextContinuationToken"):
				field = t.Name.Local
				text.Reset()
			}
		case xml.CharData:
			if field != "" {
				text.Write(t)
			}
		case xml.EndElement:
			if depth == 0 || open[depth-1] != t.Name {
				return entries, truncated, next, fmt.Errorf("unexpected closing element %q", t.Name.Local)
			}
			switch {
			case field != "":
				if perr := setListField(&entry, &truncated, &next, field, text.String()); perr != nil {
					return entries, truncated, next, perr
				}
				field = ""
			case inContents && depth == 2:
				entries = append(entries, entry)
				inContents = false
			case depth == 1:
				// Trailing bytes belong to the drain, not the decoder.
				return entries, truncated, next, nil
			}
			depth--
		}
	}
}

var errTokenTooLong = fmt.Errorf("element exceeds %d bytes", maxListTokenBytes)

// spanReader limits bytes consumed between tokens, plus one buffered read.
type spanReader struct {
	r     io.Reader
	n     int64
	allow int64
}

func (s *spanReader) Read(p []byte) (int, error) {
	if s.n > s.allow {
		return 0, errTokenTooLong
	}
	n, err := s.r.Read(p)
	s.n += int64(n)
	return n, err
}

func (s *spanReader) mark() {
	s.allow = s.n + maxListTokenBytes
}

func setListField(entry *storage.Info, truncated *bool, next *string, field, raw string) error {
	var err error
	switch field {
	case "Key":
		entry.Key = raw
	case "Size":
		entry.Size, err = strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	case "LastModified":
		entry.ModTime, err = time.Parse(time.RFC3339, strings.TrimSpace(raw))
	case "IsTruncated":
		*truncated, err = strconv.ParseBool(strings.TrimSpace(raw))
	case "NextContinuationToken":
		*next = raw
	}
	if err != nil {
		return fmt.Errorf("invalid %s: %w", field, err)
	}
	return nil
}

// drainFor reads at most DrainLimit; an incompletely read connection is not reused.
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
		// DrainLimit must not determine whether a valid conflict body is recognized.
		decodeErr := xml.NewDecoder(io.LimitReader(resp.Body, maxErrorDocBytes)).Decode(&e)
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

// knownLength passes known-length readers through and spools all others under
// MaxSpoolBytes.
func (s *Store) knownLength(ctx context.Context, r io.Reader) (io.Reader, int64, func() error, error) {
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
	spool, err := os.CreateTemp(s.spoolDir, "storage-s3-put-*")
	if err != nil {
		return nil, 0, none, err
	}
	cleanup := func() error {
		return errors.Join(spool.Close(), os.Remove(spool.Name()))
	}
	if err := spoolCopy(ctx, spool, r, s.maxSpoolBytes); err != nil {
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

// spoolCopy reads one byte past the limit to distinguish an exact fit from overflow.
func spoolCopy(ctx context.Context, dst io.Writer, src io.Reader, limit int64) error {
	read := limit
	if read < math.MaxInt64 {
		read++
	}
	capped := io.LimitReader(src, read)
	buf := make([]byte, 32*1024)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := capped.Read(buf)
		if n > 0 {
			if written += int64(n); written > limit {
				return fmt.Errorf("%w: unknown-length body exceeds the %d byte spool limit", storage.ErrTooLarge, limit)
			}
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
