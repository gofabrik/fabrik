package validation_test

import (
	"math"
	"regexp"
	"strings"
	"testing"

	v "github.com/gofabrik/fabrik/validation"
)

func TestRequired(t *testing.T) {
	for _, s := range []string{"", "   ", "\t\n"} {
		if v.Required()(s) == nil {
			t.Errorf("Required(%q) = nil, want error", s)
		}
	}
	if v.Required()("x") != nil {
		t.Error("Required(\"x\") should pass")
	}
}

func TestEmail(t *testing.T) {
	if v.Email()("") != nil {
		t.Error("Email empty should pass (presence is Required's job)")
	}
	if v.Email()("a@b.com") != nil {
		t.Error("valid email rejected")
	}
	for _, bad := range []string{
		"nope", "a@", "@b.com", "a b@c.com",
		"a@b",            // dotless domain
		"x@localhost",    // no dot
		"Bob <a@b.com>",  // display-name form
		"a@b.com (note)", // comment form
	} {
		if v.Email()(bad) == nil {
			t.Errorf("Email(%q) = nil, want error", bad)
		}
	}
}

func TestURL(t *testing.T) {
	if v.URL()("") != nil {
		t.Error("URL empty should pass")
	}
	if v.URL()("https://example.com/x") != nil {
		t.Error("valid URL rejected")
	}
	for _, bad := range []string{"example.com", "/just/a/path", "://broken"} {
		if v.URL()(bad) == nil {
			t.Errorf("URL(%q) = nil, want error", bad)
		}
	}
}

func TestMinMaxLen(t *testing.T) {
	if v.MinLen(8)("") != nil {
		t.Error("MinLen empty should pass")
	}
	if v.MinLen(3)("ab") == nil {
		t.Error("MinLen(3) on 2 chars should fail")
	}
	if v.MinLen(3)("abc") != nil {
		t.Error("MinLen(3) on 3 chars should pass")
	}
	// Rune-aware, not byte-aware.
	if v.MinLen(3)("é€京") != nil {
		t.Error("MinLen(3) on 3 runes should pass")
	}
	if v.MaxLen(2)("abc") == nil {
		t.Error("MaxLen(2) on 3 chars should fail")
	}
}

func TestMinMax_Numeric(t *testing.T) {
	if v.Min(18)(17) == nil || v.Min(18)(18) != nil {
		t.Error("Min(18) boundary wrong")
	}
	if v.Max(65535)(70000) == nil || v.Max(65535)(80) != nil {
		t.Error("Max(65535) boundary wrong")
	}
	if v.Min(1.5)(1.0) == nil || v.Min(1.5)(2.0) != nil {
		t.Error("Min on float wrong")
	}
	// NaN fails both bounds.
	if v.Min(1.5)(math.NaN()) == nil {
		t.Error("Min should reject NaN")
	}
	if v.Max(1.5)(math.NaN()) == nil {
		t.Error("Max should reject NaN")
	}
}

func TestIn(t *testing.T) {
	r := v.In("http", "tcp", "keyword")
	if r("http") != nil {
		t.Error("'http' should be allowed")
	}
	if r("ftp") == nil {
		t.Error("'ftp' should be rejected")
	}
	if v.In(1, 2, 3)(4) == nil {
		t.Error("In(ints) should reject 4")
	}
	// Message uses a comma list, not Go slice formatting.
	if msg := r("ftp").Error(); !strings.Contains(msg, "http, tcp, keyword") {
		t.Errorf("In message = %q, want comma-joined list", msg)
	}
}

func TestPattern_NilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Pattern(nil) should panic at construction")
		}
	}()
	v.Pattern(nil)
}

func TestPattern(t *testing.T) {
	re := regexp.MustCompile(`^\d{3}$`)
	if v.Pattern(re)("") != nil {
		t.Error("Pattern empty should pass")
	}
	if v.Pattern(re)("123") != nil {
		t.Error("Pattern '123' should pass")
	}
	if v.Pattern(re)("12") == nil {
		t.Error("Pattern '12' should fail")
	}
}
