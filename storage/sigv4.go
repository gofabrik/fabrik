package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// emptyPayloadSHA is the SHA-256 digest of an empty payload.
const emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// unsignedPayload defers body integrity to TLS; S3 and MinIO accept it.
const unsignedPayload = "UNSIGNED-PAYLOAD"

type credentials struct {
	accessKey string
	secretKey string
	region    string
}

// signV4 signs req in place with AWS Signature Version 4 for S3, using
// payloadSHA as a body digest or sentinel.
func signV4(req *http.Request, creds credentials, payloadSHA string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := now.UTC().Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadSHA)

	headers := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	// Range is optional; the three required headers are always signed.
	for h := range req.Header {
		lh := strings.ToLower(h)
		if lh == "range" {
			headers = append(headers, lh)
		}
	}
	sort.Strings(headers)
	var canonHeaders strings.Builder
	for _, h := range headers {
		v := req.Header.Get(h)
		if h == "host" {
			v = req.Host
			if v == "" {
				v = req.URL.Host
			}
		}
		canonHeaders.WriteString(h + ":" + strings.TrimSpace(v) + "\n")
	}
	signedHeaders := strings.Join(headers, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		canonicalQuery(req.URL),
		canonHeaders.String(),
		signedHeaders,
		payloadSHA,
	}, "\n")

	scope := strings.Join([]string{date, creds.region, "s3", "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	k := hmacSHA256([]byte("AWS4"+creds.secretKey), date)
	k = hmacSHA256(k, creds.region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(k, stringToSign))

	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+creds.accessKey+"/"+scope+
			", SignedHeaders="+signedHeaders+
			", Signature="+signature)
}

// canonicalURI applies S3's RFC 3986 escaping while preserving slashes.
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		segs[i] = uriEscape(s)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(u *url.URL) string {
	q := u.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := q[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEscape(k)+"="+uriEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

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

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
