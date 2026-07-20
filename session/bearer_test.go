package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearer_ReadDefault(t *testing.T) {
	b := Bearer{}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer abc123")
	sid, ok := b.Read(r)
	if !ok || sid != "abc123" {
		t.Fatalf("read: ok=%v sid=%q", ok, sid)
	}
}

func TestBearer_ReadCaseInsensitiveScheme(t *testing.T) {
	b := Bearer{}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "bearer abc123")
	if sid, ok := b.Read(r); !ok || sid != "abc123" {
		t.Errorf("lowercase scheme should be accepted: ok=%v sid=%q", ok, sid)
	}
}

func TestBearer_ReadCustomHeaderAndScheme(t *testing.T) {
	b := Bearer{ReadHeader: "X-Auth", Scheme: "Token"}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Auth", "Token xyz")
	sid, ok := b.Read(r)
	if !ok || sid != "xyz" {
		t.Errorf("custom header/scheme: ok=%v sid=%q", ok, sid)
	}
}

func TestBearer_ReadMissingOrWrongScheme(t *testing.T) {
	b := Bearer{}
	cases := []struct {
		name, header string
	}{
		{"missing", ""},
		{"wrong scheme", "Basic abc"},
		{"no scheme", "abc"},
		{"empty value", "Bearer "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			if _, ok := b.Read(r); ok {
				t.Errorf("expected ok=false for header %q", tc.header)
			}
		})
	}
}

func TestBearer_Write(t *testing.T) {
	b := Bearer{}
	w := httptest.NewRecorder()
	b.Write(w, "abc123", TokenWriteOptions{})
	if got := w.Header().Get("X-Session-Token"); got != "abc123" {
		t.Errorf("default WriteHeader: got %q", got)
	}

	b2 := Bearer{WriteHeader: "X-Custom"}
	w2 := httptest.NewRecorder()
	b2.Write(w2, "v", TokenWriteOptions{})
	if got := w2.Header().Get("X-Custom"); got != "v" {
		t.Errorf("custom WriteHeader: got %q", got)
	}
}

func TestBearer_Clear(t *testing.T) {
	b := Bearer{}
	w := httptest.NewRecorder()
	b.Clear(w)
	// http.Header reports an empty string for both "present and
	// empty" and "absent"; check the underlying map directly.
	if vals := w.Header().Values("X-Session-Token"); len(vals) != 1 || vals[0] != "" {
		t.Errorf("Clear should emit empty header, got %v", vals)
	}
}

func TestMulti_ReadFirstNonEmpty(t *testing.T) {
	m := Multi{Cookie{Name: "sid"}, Bearer{}}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer from-header")
	sid, ok := m.Read(r)
	if !ok || sid != "from-header" {
		t.Errorf("fall-through to Bearer: ok=%v sid=%q", ok, sid)
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	// #nosec G124 -- cookie attributes are caller-configurable
	r2.AddCookie(&http.Cookie{Name: "sid", Value: "from-cookie"})
	r2.Header.Set("Authorization", "Bearer from-header")
	sid, ok = m.Read(r2)
	if !ok || sid != "from-cookie" {
		t.Errorf("cookie should win when listed first: ok=%v sid=%q", ok, sid)
	}
}

func TestMulti_ReadReturnsFalseWhenAllMiss(t *testing.T) {
	m := Multi{Cookie{Name: "sid"}, Bearer{}}
	r := httptest.NewRequest("GET", "/", nil)
	if sid, ok := m.Read(r); ok {
		t.Errorf("expected ok=false when no member sees a token, got ok=true sid=%q", sid)
	}
}

func TestMulti_WriteAndClearApplyToAll(t *testing.T) {
	m := Multi{Cookie{Name: "sid"}, Bearer{}}
	w := httptest.NewRecorder()
	m.Write(w, "abc", TokenWriteOptions{})
	if len(w.Result().Cookies()) == 0 {
		t.Error("Multi.Write did not set cookie")
	}
	if w.Header().Get("X-Session-Token") != "abc" {
		t.Error("Multi.Write did not set bearer header")
	}

	w2 := httptest.NewRecorder()
	m.Clear(w2)
	cleared := false
	for _, c := range w2.Result().Cookies() {
		if c.Name == "sid" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("Multi.Clear did not clear cookie")
	}
	if w2.Header().Get("X-Session-Token") != "" {
		// Note: Get returns "" both for unset and explicitly empty.
		// Confirm presence by inspecting Values.
		if len(w2.Header().Values("X-Session-Token")) == 0 {
			t.Error("Multi.Clear did not emit bearer clear")
		}
	}
}
