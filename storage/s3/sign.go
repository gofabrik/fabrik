package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// unsignedPayload defers body integrity to TLS; S3 and MinIO accept it.
const unsignedPayload = "UNSIGNED-PAYLOAD"

type credentials struct {
	accessKey    string
	secretKey    string
	sessionToken string
	region       string
}

// Request URLs are already escaped; escaping again changes the S3 signature.
var signer = v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })

func signS3(req *http.Request, creds credentials, payloadSHA string, now time.Time) error {
	// The digest header must be present when the signer builds the canonical request.
	req.Header.Set("x-amz-content-sha256", payloadSHA)
	return signer.SignHTTP(req.Context(), aws.Credentials{
		AccessKeyID:     creds.accessKey,
		SecretAccessKey: creds.secretKey,
		SessionToken:    creds.sessionToken,
	}, req, payloadSHA, "s3", creds.region, now)
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
