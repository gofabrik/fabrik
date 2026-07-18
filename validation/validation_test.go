package validation_test

import (
	"errors"
	"regexp"
	"testing"

	v "github.com/gofabrik/fabrik/validation"
)

type loginInput struct {
	Email    string
	Password string
}

func (in loginInput) Validate() v.Errors {
	return v.Check(
		v.Field("email", in.Email, v.Required(), v.Email()),
		v.Field("password", in.Password, v.Required(), v.MinLen(8)),
	)
}

func TestCheck_FieldKeyedErrors(t *testing.T) {
	errs := loginInput{Email: "nope", Password: "short"}.Validate()
	if errs.Empty() {
		t.Fatal("expected errors")
	}
	if !errs.Has("email") || errs.Get("email") != "must be a valid email address" {
		t.Errorf("email error = %q", errs.Get("email"))
	}
	if errs.Get("password") != "must be at least 8 characters" {
		t.Errorf("password error = %q", errs.Get("password"))
	}
}

func TestCheck_AllPassReturnsNil(t *testing.T) {
	errs := loginInput{Email: "a@b.com", Password: "longenough"}.Validate()
	if !errs.Empty() {
		t.Fatalf("expected no errors, got %v", errs)
	}
	if errs != nil {
		t.Errorf("clean Check should return nil map, got %#v", errs)
	}
}

func TestCheck_FirstRuleWins(t *testing.T) {
	errs := v.Check(v.Field("p", "", v.Required(), v.MinLen(8)))
	if errs.Get("p") != "is required" {
		t.Errorf("empty: got %q, want 'is required'", errs.Get("p"))
	}
	errs = v.Check(v.Field("p", "abc", v.Required(), v.MinLen(8)))
	if errs.Get("p") != "must be at least 8 characters" {
		t.Errorf("short: got %q", errs.Get("p"))
	}
}

func TestErrors_AddAndFormLevel(t *testing.T) {
	var errs v.Errors
	errs.Add("", "email or password is wrong")
	errs.Add("", "ignored second") // first wins
	errs.Add("email", "is required")
	if errs.Get("") != "email or password is wrong" {
		t.Errorf("form-level = %q", errs.Get(""))
	}
	// Form-level errors omit the field prefix; field keys sort.
	if got := errs.Error(); got != "email or password is wrong; email: is required" {
		t.Errorf("Error() = %q", got)
	}
}

func TestField_PlainFuncAndBy(t *testing.T) {
	even := func(n int) error {
		if n%2 != 0 {
			return errors.New("must be even")
		}
		return nil
	}
	errs := v.Check(
		v.Field("a", 3, even),
		v.Field("b", 4, v.By(even)),
	)
	if errs.Get("a") != "must be even" {
		t.Errorf("a = %q", errs.Get("a"))
	}
	if errs.Has("b") {
		t.Errorf("b should pass, got %q", errs.Get("b"))
	}
}

func TestField_TypeInference(t *testing.T) {
	errs := v.Check(
		v.Field("kind", "ftp", v.In("http", "tcp")),
		v.Field("port", 70000, v.Min(1), v.Max(65535)),
		v.Field("name", "ok", v.Required(), v.Pattern(regexp.MustCompile(`^[a-z]+$`))),
	)
	if errs.Get("kind") == "" || errs.Get("port") == "" {
		t.Errorf("expected kind and port errors, got %v", errs)
	}
	if errs.Has("name") {
		t.Errorf("name should pass, got %q", errs.Get("name"))
	}
}
