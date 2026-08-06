package s3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func signedHeaders(t *testing.T, req *http.Request) string {
	t.Helper()
	auth := req.Header.Get("Authorization")
	for _, part := range strings.Split(auth, ", ") {
		if after, ok := strings.CutPrefix(part, "SignedHeaders="); ok {
			return after
		}
	}
	t.Fatalf("no SignedHeaders in authorization %q", auth)
	return ""
}

func TestSignS3AWSDocVector(t *testing.T) {
	req, err := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-9")
	creds := credentials{
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", // #nosec G101 -- public AWS documentation test vector
		region:    "us-east-1",
	}
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	if err := signS3(req, creds, emptyPayloadSHA, at); err != nil {
		t.Fatal(err)
	}

	const want = "AWS4-HMAC-SHA256 " +
		"Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization =\n%q\nwant\n%q", got, want)
	}
	if got := req.Header.Get("x-amz-content-sha256"); got != emptyPayloadSHA {
		t.Fatalf("x-amz-content-sha256 = %q, want the payload digest on the request", got)
	}
}

func TestSignS3SignsContentLengthOnUpload(t *testing.T) {
	req, err := http.NewRequest("PUT", "https://s3.example.com/bkt/up.bin", strings.NewReader("body bytes"))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len("body bytes"))
	creds := credentials{accessKey: "a", secretKey: "s", region: "eu-central-1"}
	if err := signS3(req, creds, unsignedPayload, time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got := signedHeaders(t, req); got != "content-length;host;x-amz-content-sha256;x-amz-date" {
		t.Fatalf("signed headers = %q", got)
	}
}

func TestSignS3ZeroLengthUpload(t *testing.T) {
	req, err := http.NewRequest("PUT", "https://s3.example.com/bkt/empty.bin", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 0
	creds := credentials{accessKey: "a", secretKey: "s", region: "us-east-1"}
	if err := signS3(req, creds, unsignedPayload, time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got := signedHeaders(t, req); got != "host;x-amz-content-sha256;x-amz-date" {
		t.Fatalf("signed headers = %q", got)
	}
}

func TestSignS3EmitsSessionToken(t *testing.T) {
	req, err := http.NewRequest("GET", "https://s3.example.com/bkt/key.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	creds := credentials{accessKey: "a", secretKey: "s", sessionToken: "THETOKEN", region: "us-east-1"}
	if err := signS3(req, creds, emptyPayloadSHA, time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Amz-Security-Token"); got != "THETOKEN" {
		t.Fatalf("x-amz-security-token = %q", got)
	}
	if got := signedHeaders(t, req); !strings.Contains(got, "x-amz-security-token") {
		t.Fatalf("signed headers = %q, want the session token signed", got)
	}
}

// The expected signature is cross-checked against the retired in-tree signer
// and flips if the path is escaped a second time.
func TestSignS3EscapedPathVector(t *testing.T) {
	req, err := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/my%20folder/a%2Bb.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	creds := credentials{
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
	}
	if err := signS3(req, creds, emptyPayloadSHA, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date, " +
		"Signature=7e6ff5f90a3632da1ffd6c43407ffb9513fd8be59b14d597b62b697e2a3b6a92"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization =\n%q\nwant\n%q", got, want)
	}
}

func TestSessionTokenFlowsFromOptions(t *testing.T) {
	var gotToken, gotSigned string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Amz-Security-Token")
		for _, part := range strings.Split(r.Header.Get("Authorization"), ", ") {
			if after, ok := strings.CutPrefix(part, "SignedHeaders="); ok {
				gotSigned = after
			}
		}
	}))
	defer srv.Close()
	s, err := New(Options{
		Endpoint: srv.URL, Bucket: "bkt", AccessKey: "a", SecretKey: "s",
		SessionToken: "THETOKEN", AllowInsecure: true, Client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(t.Context(), "k"); err != nil {
		t.Fatal(err)
	}
	if gotToken != "THETOKEN" {
		t.Fatalf("x-amz-security-token on the wire = %q", gotToken)
	}
	if !strings.Contains(gotSigned, "x-amz-security-token") {
		t.Fatalf("signed headers on the wire = %q, want the session token signed", gotSigned)
	}
}
