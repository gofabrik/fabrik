package password

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Credentials is the (email, password) pair the [Parser] extracts
// from a request body.
type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Parser extracts [Credentials] from a request body. Returns an
// error on malformed input; the provider translates that error
// into [Options.OnFailure].
type Parser func(r *http.Request) (Credentials, error)

// defaultMultipartMemory bounds in-memory multipart parsing; the
// overall body is already capped by http.MaxBytesReader upstream.
const defaultMultipartMemory = 1 << 20 // 1 MiB

// DefaultParser auto-detects the request body shape by
// Content-Type:
//
//   - application/json: decode as {"email": "...", "password": "..."}.
//   - application/x-www-form-urlencoded: ParseForm and read the
//     "email" + "password" fields.
//   - multipart/form-data: ParseMultipartForm and read the same
//     fields.
//   - other / missing Content-Type: return [ErrUnsupportedContentType].
//
// Override [Options.Parser] for custom field names ("username" /
// "passwd") or non-standard body shapes.
func DefaultParser(r *http.Request) (Credentials, error) {
	ct := r.Header.Get("Content-Type")
	// Strip any "; charset=..." suffix.
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))

	switch ct {
	case "application/json":
		dec := json.NewDecoder(r.Body)
		var c Credentials
		if err := dec.Decode(&c); err != nil {
			return Credentials{}, err
		}
		// Reject trailing garbage: an auth body is exactly one JSON
		// object, nothing after it.
		if dec.More() {
			return Credentials{}, errors.New("password: unexpected data after JSON body")
		}
		return c, nil
	case "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			return Credentials{}, err
		}
		return Credentials{
			Email:    r.PostForm.Get("email"),
			Password: r.PostForm.Get("password"),
		}, nil
	case "multipart/form-data":
		// ParseForm does not parse a multipart body; call
		// ParseMultipartForm directly, or PostForm stays empty.
		if err := r.ParseMultipartForm(defaultMultipartMemory); err != nil {
			return Credentials{}, err
		}
		return Credentials{
			Email:    r.PostForm.Get("email"),
			Password: r.PostForm.Get("password"),
		}, nil
	default:
		return Credentials{}, ErrUnsupportedContentType
	}
}

// ErrUnsupportedContentType is returned by [DefaultParser] when the
// request's Content-Type isn't one of the recognised body shapes.
var ErrUnsupportedContentType = errors.New("password: unsupported content type")

// ErrInvalidCredentials is the generic auth-failure error the
// provider hands to [Options.OnFailure]. The handler should NOT
// surface different errors for "user not found" vs "wrong
// password" - both leak whether the email is registered. The
// default OnFailure writes a generic 401 regardless of cause.
var ErrInvalidCredentials = errors.New("password: invalid credentials")

// ErrPasswordTooShort is the sentinel returned to [Options.OnFailure]
// when a registration attempt's password is shorter than the
// configured [Options.MinPasswordLength]. The concrete value handed
// to OnFailure is a [*PasswordTooShortError] which satisfies
// errors.Is(err, ErrPasswordTooShort) and carries the configured
// minimum for app-side inspection - without leaking that minimum
// through Error().
//
// Custom OnFailure handlers that surface err.Error() directly will
// see only "password: too short", not "must be at least 12
// characters" - preserving the no-information-disclosure discipline
// the rest of the package follows. Handlers that want to render the
// configured minimum in their UI should pull it from the typed error
// via errors.As:
//
//	var pse *password.PasswordTooShortError
//	if errors.As(err, &pse) {
//	    fmt.Fprintf(w, "min %d characters required", pse.Min)
//	}
var ErrPasswordTooShort = errors.New("password: too short")

// PasswordTooShortError is the concrete error type returned to
// [Options.OnFailure] when the registration password is too short.
// Use errors.As to extract Min if a custom handler needs to render
// the configured minimum length. errors.Is(err, ErrPasswordTooShort)
// also works via the Unwrap chain.
type PasswordTooShortError struct {
	Min int // the configured Options.MinPasswordLength at the time of the failure
}

// Error returns ErrPasswordTooShort.Error() - the configured Min is
// deliberately NOT included so that handlers writing err.Error() to
// the wire do not leak policy details.
func (e *PasswordTooShortError) Error() string { return ErrPasswordTooShort.Error() }

// Unwrap exposes ErrPasswordTooShort so errors.Is(err,
// ErrPasswordTooShort) returns true.
func (e *PasswordTooShortError) Unwrap() error { return ErrPasswordTooShort }

// ErrPasswordTooLong is the sentinel for a registration password
// beyond the hasher's limit (bcrypt caps at 72 bytes). Like
// [ErrPasswordTooShort] it is a policy rejection, not a server error.
var ErrPasswordTooLong = errors.New("password: too long")

// PasswordTooLongError carries the maximum byte length via errors.As,
// and satisfies errors.Is(err, ErrPasswordTooLong).
type PasswordTooLongError struct {
	Max int
}

func (e *PasswordTooLongError) Error() string { return ErrPasswordTooLong.Error() }
func (e *PasswordTooLongError) Unwrap() error { return ErrPasswordTooLong }
