package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testS3(t *testing.T, region string, handler http.HandlerFunc) *Store {
	t.Helper()
	return testS3Opts(t, Options{Region: region}, handler)
}

// testS3Opts fills in the endpoint and credentials so a test states only the
// options it exercises.
func testS3Opts(t *testing.T, opts Options, handler http.HandlerFunc) *Store {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	// Client() initializes the server; URL is empty until it runs.
	client := srv.Client()
	opts.Endpoint, opts.Bucket = srv.URL, "bkt"
	opts.AccessKey, opts.SecretKey = "a", "s"
	opts.AllowInsecure = true
	if opts.Client == nil {
		opts.Client = client
	}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateBucketSendsLocationConstraint(t *testing.T) {
	var gotBody string
	s := testS3(t, "eu-central-1", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read create-bucket body: %v", err)
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	})
	if err := s.CreateBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	const want = "<CreateBucketConfiguration><LocationConstraint>eu-central-1</LocationConstraint></CreateBucketConfiguration>"
	if gotBody != want {
		t.Fatalf("regional create body =\n%q\nwant\n%q", gotBody, want)
	}
}

func TestCreateBucketConflictSemantics(t *testing.T) {
	owned := testS3(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		if _, err := io.WriteString(w, `<Error><Code>BucketAlreadyOwnedByYou</Code></Error>`); err != nil {
			t.Errorf("write owned-bucket response: %v", err)
		}
	})
	if err := owned.CreateBucket(context.Background()); err != nil {
		t.Fatalf("owned bucket must succeed: %v", err)
	}
	taken := testS3(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		if _, err := io.WriteString(w, `<Error><Code>BucketAlreadyExists</Code></Error>`); err != nil {
			t.Errorf("write existing-bucket response: %v", err)
		}
	})
	if err := taken.CreateBucket(context.Background()); err == nil {
		t.Fatal("foreign-owned bucket conflict must error")
	}
}

func TestRedirectsAreRefused(t *testing.T) {
	// Redirect within the test server so a followed request reaches /steal.
	var stolen int
	s := testS3(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/steal" {
			stolen++
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/steal", http.StatusTemporaryRedirect)
	})
	// Redirects must not be followed and replay a signed request elsewhere.
	if err := s.Put(context.Background(), "k", strings.NewReader("x")); err == nil {
		t.Fatal("redirected put must fail")
	}
	if _, err := s.Open(context.Background(), "k"); err == nil {
		t.Fatal("redirected open must fail")
	}
	if stolen != 0 {
		t.Fatalf("redirect target received %d requests; must receive none", stolen)
	}
}

func TestPutSendsKnownContentLength(t *testing.T) {
	var got int64 = -1
	var chunked []string
	s := testS3(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		got = r.ContentLength
		chunked = r.TransferEncoding
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("drain upload body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})
	pr, pw := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := pw.Write([]byte("stream me"))
		writeDone <- errors.Join(writeErr, pw.Close())
	}()
	if err := s.Put(context.Background(), "k", pr); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write pipe upload: %v", err)
	}
	if got != int64(len("stream me")) || len(chunked) != 0 {
		t.Fatalf("pipe upload: Content-Length=%d TransferEncoding=%v (AWS needs a known length)", got, chunked)
	}
}

func TestPutSendsEmptyObjectLength(t *testing.T) {
	var got int64 = -2
	var chunked []string
	s := testS3(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		got = r.ContentLength
		chunked = r.TransferEncoding
		w.WriteHeader(http.StatusOK)
	})
	past, err := os.CreateTemp(t.TempDir(), "past-eof")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := past.Close(); err != nil {
			t.Errorf("close past-EOF fixture: %v", err)
		}
	})
	if _, err := past.WriteString("abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := past.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// Zero-byte sources must use Content-Length: 0, not unknown-length framing.
	for name, r := range map[string]io.Reader{
		"sized":    strings.NewReader(""),
		"spooled":  struct{ io.Reader }{strings.NewReader("")},
		"past-eof": past,
	} {
		got = -2
		if err := s.Put(context.Background(), "k", r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != 0 || len(chunked) != 0 {
			t.Fatalf("%s empty upload: Content-Length=%d TransferEncoding=%v", name, got, chunked)
		}
	}
}

func TestPutDoesNotCloseCallerFile(t *testing.T) {
	s := testS3(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("drain upload body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})
	f, err := os.CreateTemp(t.TempDir(), "body")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("close upload fixture: %v", err)
		}
	})
	if _, err := f.WriteString("file body"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "k", f); err != nil {
		t.Fatal(err)
	}
	// Shield the file because net/http closes io.ReadCloser request bodies.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("caller's file closed by Put: %v", err)
	}
	if _, err := f.Stat(); err != nil {
		t.Fatalf("caller's file unusable after Put: %v", err)
	}
}

func TestNewRejectsBadEndpointsAndBuckets(t *testing.T) {
	base := Options{Bucket: "bkt", AccessKey: "a", SecretKey: "s", AllowInsecure: true}
	for _, ep := range []string{
		"http://user:pw@host:9000",
		"http://host:9000/base/path",
		"http://host:9000?x=1",
		"http://host:9000#frag",
		"ftp://host",
		"http://",
		"http://host:9000?",
		"http://host:9000#",
		"http://host:9000/",
		"http://:9000",
	} {
		o := base
		o.Endpoint = ep
		if _, err := New(o); err == nil {
			t.Fatalf("endpoint %q accepted", ep)
		}
	}
	for _, b := range []string{"ab", "UPPER", "has_underscore", "-lead", "trail-", "a..b", "192.168.0.1", strings.Repeat("x", 64)} {
		o := base
		o.Endpoint = "http://host:9000"
		o.Bucket = b
		if _, err := New(o); err == nil {
			t.Fatalf("bucket %q accepted", b)
		}
	}
	for _, b := range []string{"bkt", "123", "12.34", "999.1.1.1", "1.2.3.4.5"} {
		o := base
		o.Endpoint = "http://host:9000"
		o.Bucket = b
		if _, err := New(o); err != nil {
			t.Fatalf("valid bucket %q rejected: %v", b, err)
		}
	}
}

// testBudget is short enough to keep the bound tests quick and long enough to
// survive a loaded machine.
const testBudget = 100 * time.Millisecond

// stall blocks until the client abandons the exchange. The ceiling keeps a
// failing test from wedging the server's shutdown.
func stall(r *http.Request) {
	// The server only watches for a closed connection once the handler is done
	// with the request body, so an upload has to be consumed first.
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

// bounded runs op off the test goroutine so an unbounded operation fails the
// test instead of hanging it.
func bounded(t *testing.T, op func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- op() }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("operation did not return; nothing bounded it")
		return nil
	}
}

func TestOperationBudgetBoundsStalledHandshake(t *testing.T) {
	// Every operation gives up at its own budget when the endpoint accepts the
	// request and never answers. List budgets each page exchange.
	ops := []struct {
		name string
		run  func(context.Context, *Store) error
	}{
		{"put", func(ctx context.Context, s *Store) error { return s.Put(ctx, "k", strings.NewReader("x")) }},
		{"open", func(ctx context.Context, s *Store) error { _, err := s.Open(ctx, "k"); return err }},
		{"stat", func(ctx context.Context, s *Store) error { _, err := s.Stat(ctx, "k"); return err }},
		{"delete", func(ctx context.Context, s *Store) error { return s.Delete(ctx, "k") }},
		{"list", func(ctx context.Context, s *Store) error {
			for _, err := range s.List(ctx, "") {
				if err != nil {
					return err
				}
			}
			return nil
		}},
		{"create bucket", func(ctx context.Context, s *Store) error { return s.CreateBucket(ctx) }},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			s := testS3Opts(t, Options{OperationTimeout: testBudget}, func(w http.ResponseWriter, r *http.Request) {
				stall(r)
			})
			start := time.Now()
			err := bounded(t, func() error { return op.run(context.Background(), s) })
			if err == nil {
				t.Fatal("stalled handshake must fail")
			}
			if elapsed := time.Since(start); elapsed < testBudget {
				t.Fatalf("failed after %v, before the %v budget: %v", elapsed, testBudget, err)
			}
			if prefix := "storage: " + op.name; !strings.HasPrefix(err.Error(), prefix) {
				t.Fatalf("error %q lost the %q prefix", err, prefix)
			}
		})
	}
}

func TestOperationBudgetBoundsStalledBody(t *testing.T) {
	// The budget covers draining and decoding too, on success and error status
	// alike. Stat is absent because a HEAD response has no body to stall; its
	// handshake bound is pinned above.
	cases := []struct {
		name   string
		status int
		run    func(context.Context, *Store) error
	}{
		{"put", http.StatusOK, func(ctx context.Context, s *Store) error { return s.Put(ctx, "k", strings.NewReader("x")) }},
		{"put", http.StatusInternalServerError, func(ctx context.Context, s *Store) error { return s.Put(ctx, "k", strings.NewReader("x")) }},
		{"open", http.StatusNotFound, func(ctx context.Context, s *Store) error { _, err := s.Open(ctx, "k"); return err }},
		{"open", http.StatusInternalServerError, func(ctx context.Context, s *Store) error { _, err := s.Open(ctx, "k"); return err }},
		{"delete", http.StatusOK, func(ctx context.Context, s *Store) error { return s.Delete(ctx, "k") }},
		{"delete", http.StatusInternalServerError, func(ctx context.Context, s *Store) error { return s.Delete(ctx, "k") }},
		{"list", http.StatusOK, func(ctx context.Context, s *Store) error {
			for _, err := range s.List(ctx, "") {
				if err != nil {
					return err
				}
			}
			return nil
		}},
		{"list", http.StatusInternalServerError, func(ctx context.Context, s *Store) error {
			for _, err := range s.List(ctx, "") {
				if err != nil {
					return err
				}
			}
			return nil
		}},
		{"create bucket", http.StatusOK, func(ctx context.Context, s *Store) error { return s.CreateBucket(ctx) }},
		{"create bucket", http.StatusConflict, func(ctx context.Context, s *Store) error { return s.CreateBucket(ctx) }},
		{"create bucket", http.StatusInternalServerError, func(ctx context.Context, s *Store) error { return s.CreateBucket(ctx) }},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%d", tc.name, tc.status), func(t *testing.T) {
			s := testS3Opts(t, Options{OperationTimeout: testBudget}, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.(http.Flusher).Flush()
				stall(r)
			})
			start := time.Now()
			err := bounded(t, func() error { return tc.run(context.Background(), s) })
			if err == nil {
				t.Fatal("stalled body must fail")
			}
			if elapsed := time.Since(start); elapsed < testBudget {
				t.Fatalf("failed after %v, before the %v budget: %v", elapsed, testBudget, err)
			}
			if prefix := "storage: " + tc.name; !strings.HasPrefix(err.Error(), prefix) {
				t.Fatalf("error %q lost the %q prefix", err, prefix)
			}
		})
	}
}

func TestDrainStopsAtDrainLimit(t *testing.T) {
	// An oversized error body is abandoned at the cap: the endpoint cannot push
	// the rest of it, and the connection is spent because net/http will not
	// reuse one whose body was left unread.
	const limit = 16
	for _, tc := range []struct {
		name      string
		body      int
		wantConns int64
		wantWhole bool
	}{
		{"within the cap", limit / 2, 1, true},
		{"over the cap", 1 << 20, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conns, wrote atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				// A write cut short by the client is what the cap is for.
				n, _ := io.WriteString(w, strings.Repeat("x", tc.body))
				wrote.Add(int64(n))
			}))
			srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					conns.Add(1)
				}
			}
			srv.Start()
			t.Cleanup(srv.Close)
			s, err := New(Options{
				Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "s",
				AllowInsecure: true, Client: srv.Client(), DrainLimit: limit,
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := s.Delete(context.Background(), "k"); err == nil {
					t.Fatal("s3 status 500 must fail")
				}
			}
			if got := conns.Load(); got != tc.wantConns {
				t.Fatalf("two requests opened %d connections, want %d", got, tc.wantConns)
			}
			if whole := wrote.Load() == int64(2*tc.body); whole != tc.wantWhole {
				t.Fatalf("endpoint wrote %d of %d body bytes", wrote.Load(), 2*tc.body)
			}
		})
	}
}

func TestOpenDetachesSlowBodyFromBudget(t *testing.T) {
	// The budget covers the handshake only; a download slower than the budget
	// must read to the end.
	const chunks = 5
	s := testS3Opts(t, Options{OperationTimeout: testBudget}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for range chunks {
			time.Sleep(testBudget / 2)
			if _, err := io.WriteString(w, "chunk;"); err != nil {
				t.Errorf("write chunk: %v", err)
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	rc, err := s.Open(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("body slower than the budget must read fully: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("chunk;", chunks); string(body) != want {
		t.Fatalf("read %q, want %q", body, want)
	}
}

func TestOpenBudgetExpiryIsDeadlineExceeded(t *testing.T) {
	// Budget expiry classifies as a deadline, not as a cancellation, so callers
	// can tell it from their own.
	s := testS3Opts(t, Options{OperationTimeout: testBudget}, func(w http.ResponseWriter, r *http.Request) {
		stall(r)
	})
	err := bounded(t, func() error { _, err := s.Open(context.Background(), "k"); return err })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("budget expiry = %v, want a context.DeadlineExceeded error", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("budget expiry reported as caller cancellation: %v", err)
	}
	if prefix := `storage: open "k":`; !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("error %q lost the %q prefix", err, prefix)
	}
}

func TestOpenCallerCancelPassesThrough(t *testing.T) {
	// Caller cancellation must reach the caller unchanged, not disguised as the
	// package's budget.
	s := testS3Opts(t, Options{OperationTimeout: 5 * time.Second}, func(w http.ResponseWriter, r *http.Request) {
		stall(r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(testBudget, cancel)
	err := bounded(t, func() error { _, err := s.Open(ctx, "k"); return err })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation = %v, want a context.Canceled error", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller cancellation reported as budget expiry: %v", err)
	}
}

// captureCtx reports the context the transport sees, which is the store's
// per-operation context.
type captureCtx struct {
	rt  http.RoundTripper
	ctx chan context.Context
}

func (c captureCtx) RoundTrip(r *http.Request) (*http.Response, error) {
	c.ctx <- r.Context()
	return c.rt.RoundTrip(r)
}

func TestOpenReleasesContextOnClose(t *testing.T) {
	// The detached body keeps the operation context alive, so its Close must
	// release it.
	reqCtx := make(chan context.Context, 1)
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, "payload"); err != nil {
			t.Errorf("write body: %v", err)
		}
	}))
	client := &http.Client{Transport: captureCtx{rt: srv.Client().Transport, ctx: reqCtx}}
	s, err := New(Options{
		Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "s",
		AllowInsecure: true, Client: client, OperationTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := s.Open(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ctx := <-reqCtx
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("operation context canceled while the body was still open")
	default:
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Close left the operation context running")
	}
}

func TestNewRejectsNegativeBounds(t *testing.T) {
	base := Options{
		Endpoint: "http://host:9000", Bucket: "bkt",
		AccessKey: "a", SecretKey: "s", AllowInsecure: true,
	}
	if _, err := New(base); err != nil {
		t.Fatalf("zero bounds must default: %v", err)
	}
	negativeTimeout := base
	negativeTimeout.OperationTimeout = -time.Second
	if _, err := New(negativeTimeout); err == nil {
		t.Fatal("negative OperationTimeout accepted")
	}
	negativeDrain := base
	negativeDrain.DrainLimit = -1
	if _, err := New(negativeDrain); err == nil {
		t.Fatal("negative DrainLimit accepted")
	}
}

// TestOpenErrorDrainClassification pins that a drain interrupted mid error
// body keeps the budget-vs-caller distinction: expiry reads as
// DeadlineExceeded, a caller cancel as Canceled and never as the budget.
func TestOpenErrorDrainClassification(t *testing.T) {
	newStalled := func() *Store {
		return testS3Opts(t, Options{OperationTimeout: testBudget}, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "<Error><Code>Internal")
			w.(http.Flusher).Flush()
			stall(r)
		})
	}

	t.Run("budget expiry", func(t *testing.T) {
		_, err := newStalled().Open(context.Background(), "k")
		if err == nil {
			t.Fatal("stalled error drain must fail")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the budget as DeadlineExceeded", err)
		}
	})

	t.Run("caller cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(testBudget/4, cancel)
		_, err := newStalled().Open(ctx, "k")
		if err == nil {
			t.Fatal("canceled error drain must fail")
		}
		if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want caller cancellation, not the budget", err)
		}
	})
}

// TestDrainReadsExactlyTheCap counts the bytes the client reads from each
// oversized response body: a pure drain reads exactly DrainLimit (a smaller or
// omitted drain fails the floor, a larger one the ceiling), and the 409 paths
// read exactly twice the cap: a capped decode, then the capped release. Each
// case also asserts the operation outcome, so a drain skipped by an early
// return cannot pass. List's oversized 200 page is exercised with the decode
// cap in the list-bounds increment; its error path is pinned here.
func TestDrainReadsExactlyTheCap(t *testing.T) {
	const limit = 128
	ownedXML := "<Error><Code>BucketAlreadyOwnedByYou</Code></Error>"
	garbage := strings.Repeat("x", 1<<20)
	cases := []struct {
		name     string
		status   int
		body     string
		wantRead int64
		wantErr  bool
		run      func(context.Context, *Store) error
	}{
		{"put trailing success", http.StatusOK, garbage, limit, false, func(ctx context.Context, s *Store) error { return s.Put(ctx, "k", strings.NewReader("v")) }},
		{"put error", http.StatusInternalServerError, garbage, limit, true, func(ctx context.Context, s *Store) error { return s.Put(ctx, "k", strings.NewReader("v")) }},
		{"open notfound", http.StatusNotFound, garbage, limit, true, func(ctx context.Context, s *Store) error { _, err := s.Open(ctx, "k"); return err }},
		{"open error", http.StatusInternalServerError, garbage, limit, true, func(ctx context.Context, s *Store) error { _, err := s.Open(ctx, "k"); return err }},
		{"delete trailing success", http.StatusOK, garbage, limit, false, func(ctx context.Context, s *Store) error { return s.Delete(ctx, "k") }},
		{"delete error", http.StatusInternalServerError, garbage, limit, true, func(ctx context.Context, s *Store) error { return s.Delete(ctx, "k") }},
		{"list error", http.StatusInternalServerError, garbage, limit, true, func(ctx context.Context, s *Store) error {
			for _, err := range s.List(ctx, "") {
				if err != nil {
					return err
				}
			}
			return nil
		}},
		{"create bucket trailing success", http.StatusOK, garbage, limit, false, func(ctx context.Context, s *Store) error { return s.CreateBucket(ctx) }},
		{"create bucket conflict", http.StatusConflict, garbage, 2 * limit, true, func(ctx context.Context, s *Store) error { return s.CreateBucket(ctx) }},
		{"create bucket owned success", http.StatusConflict, ownedXML + garbage, 2 * limit, false, func(ctx context.Context, s *Store) error { return s.CreateBucket(ctx) }},
		{"create bucket error", http.StatusInternalServerError, garbage, limit, true, func(ctx context.Context, s *Store) error { return s.CreateBucket(ctx) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var read atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			client := srv.Client()
			client.Transport = countingTransport{base: client.Transport, read: &read}
			s, err := New(Options{
				Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "s",
				AllowInsecure: true, Client: client, DrainLimit: limit,
			})
			if err != nil {
				t.Fatal(err)
			}
			opErr := tc.run(context.Background(), s)
			if gotErr := opErr != nil; gotErr != tc.wantErr {
				t.Fatalf("operation error = %v, wantErr %v", opErr, tc.wantErr)
			}
			if got := read.Load(); got != tc.wantRead {
				t.Fatalf("client read %d body bytes, want exactly %d", got, tc.wantRead)
			}
		})
	}
}

type countingTransport struct {
	base http.RoundTripper
	read *atomic.Int64
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if resp != nil {
		resp.Body = countingBody{ReadCloser: resp.Body, read: t.read}
	}
	return resp, err
}

type countingBody struct {
	io.ReadCloser
	read *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}
