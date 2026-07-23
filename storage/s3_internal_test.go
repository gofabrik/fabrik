package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func testS3(t *testing.T, region string, handler http.HandlerFunc) *S3 {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s, err := NewS3(S3Options{
		Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "s",
		Region: region, AllowInsecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateBucketSendsLocationConstraint(t *testing.T) {
	var gotBody string
	s := testS3(t, "eu-central-1", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
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
		io.WriteString(w, `<Error><Code>BucketAlreadyOwnedByYou</Code></Error>`)
	})
	if err := owned.CreateBucket(context.Background()); err != nil {
		t.Fatalf("owned bucket must succeed: %v", err)
	}
	taken := testS3(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `<Error><Code>BucketAlreadyExists</Code></Error>`)
	})
	if err := taken.CreateBucket(context.Background()); err == nil {
		t.Fatal("foreign-owned bucket conflict must error")
	}
}

func TestRedirectsAreRefused(t *testing.T) {
	var stolen int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stolen++
	}))
	t.Cleanup(target.Close)
	s := testS3(t, "us-east-1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	})
	// Redirects must not replay a signed request to another host.
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
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("stream me"))
		pw.Close()
	}()
	if err := s.Put(context.Background(), "k", pr); err != nil {
		t.Fatal(err)
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
	defer past.Close()
	past.WriteString("abc")
	past.Seek(100, io.SeekStart)
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
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	f, err := os.CreateTemp(t.TempDir(), "body")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("file body")
	f.Seek(0, io.SeekStart)
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

func TestNewS3RejectsBadEndpointsAndBuckets(t *testing.T) {
	base := S3Options{Bucket: "bkt", AccessKey: "a", SecretKey: "s", AllowInsecure: true}
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
		if _, err := NewS3(o); err == nil {
			t.Fatalf("endpoint %q accepted", ep)
		}
	}
	for _, b := range []string{"ab", "UPPER", "has_underscore", "-lead", "trail-", "a..b", "192.168.0.1", strings.Repeat("x", 64)} {
		o := base
		o.Endpoint = "http://host:9000"
		o.Bucket = b
		if _, err := NewS3(o); err == nil {
			t.Fatalf("bucket %q accepted", b)
		}
	}
	for _, b := range []string{"bkt", "123", "12.34", "999.1.1.1", "1.2.3.4.5"} {
		o := base
		o.Endpoint = "http://host:9000"
		o.Bucket = b
		if _, err := NewS3(o); err != nil {
			t.Fatalf("valid bucket %q rejected: %v", b, err)
		}
	}
}
