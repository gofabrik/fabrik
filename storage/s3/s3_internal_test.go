package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func testS3(t *testing.T, region string, handler http.HandlerFunc) *Store {
	t.Helper()
	srv := httptest.NewTestServer(t, handler)
	// Client() initializes the server; URL is empty until it runs.
	client := srv.Client()
	s, err := New(Options{
		Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "s",
		Region: region, AllowInsecure: true, Client: client,
	})
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
