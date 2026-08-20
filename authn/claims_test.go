package authn

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestClaimSetRoundTrip(t *testing.T) {
	in := ClaimSet{
		Issuer:       "https://issuer.example",
		Subject:      "alice",
		Audience:     Audience{"api", "web"},
		Expiry:       NewNumericDate(time.Unix(1900000000, 0)),
		NotBefore:    NewNumericDate(time.Unix(1800000000, 0)),
		IssuedAt:     NewNumericDate(time.Unix(1700000000, 0)),
		ID:           "jti-1",
		Scope:        "read write",
		ClientID:     "client-1",
		Roles:        []string{"admin"},
		Groups:       []string{"staff"},
		Entitlements: []string{"billing"},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out ClaimSet
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the claims:\n in=%+v\nout=%+v", in, out)
	}
}

func TestClaimSetUsesRegisteredNames(t *testing.T) {
	full := ClaimSet{
		Issuer:       "i",
		Subject:      "s",
		Audience:     Audience{"a", "b"},
		Expiry:       NewNumericDate(time.Unix(3, 0)),
		NotBefore:    NewNumericDate(time.Unix(2, 0)),
		IssuedAt:     NewNumericDate(time.Unix(1, 0)),
		ID:           "j",
		Scope:        "sc",
		ClientID:     "c",
		Roles:        []string{"r"},
		Groups:       []string{"g"},
		Entitlements: []string{"e"},
	}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"iss": true, "sub": true, "aud": true, "exp": true, "nbf": true,
		"iat": true, "jti": true, "scope": true, "client_id": true,
		"roles": true, "groups": true, "entitlements": true,
	}
	if len(m) != len(want) {
		t.Fatalf("marshal produced %d keys, want %d: %s", len(m), len(want), raw)
	}
	for k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("marshal missing registered name %q: %s", k, raw)
		}
	}
}

func TestClaimSetEmptyMarshalsBare(t *testing.T) {
	raw, err := json.Marshal(ClaimSet{})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Fatalf("empty ClaimSet marshals with spurious fields: %s", raw)
	}
}

func TestAudienceEncodings(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Audience
	}{
		{"string form", `{"aud":"api"}`, Audience{"api"}},
		{"array form", `{"aud":["api","web"]}`, Audience{"api", "web"}},
		{"absent", `{}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c ClaimSet
			if err := json.Unmarshal([]byte(tt.in), &c); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.Audience, tt.want) {
				t.Fatalf("aud = %#v, want %#v", c.Audience, tt.want)
			}
		})
	}

	single, err := json.Marshal(ClaimSet{Audience: Audience{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(single) != `{"aud":"api"}` {
		t.Errorf("single audience marshals as %s, want the string form", single)
	}
	multi, err := json.Marshal(ClaimSet{Audience: Audience{"api", "web"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(multi) != `{"aud":["api","web"]}` {
		t.Errorf("multi audience marshals as %s, want the array form", multi)
	}
}

func TestAudienceRejectsOtherShapes(t *testing.T) {
	shapes := []string{
		`{"aud":42}`, `{"aud":{"a":1}}`, `{"aud":[1]}`, `{"aud":null}`,
		`{"aud":[null]}`, `{"aud":["api",null]}`,
	}
	for _, in := range shapes {
		var c ClaimSet
		if err := json.Unmarshal([]byte(in), &c); err == nil {
			t.Errorf("aud shape accepted: %s", in)
		}
	}
}

func ptr(d NumericDate) *NumericDate { return &d }

func TestNumericDateForms(t *testing.T) {
	var c ClaimSet
	if err := json.Unmarshal([]byte(`{"exp":1900000000.75}`), &c); err != nil {
		t.Fatalf("fractional input rejected: %v", err)
	}
	if got := float64(*c.Expiry); got != 1900000000.75 {
		t.Fatalf("fractional exp = %v, want preserved 1900000000.75", got)
	}
	if got := c.Expiry.Time().Unix(); got != 1900000000 {
		t.Fatalf("Time().Unix() = %d, want 1900000000", got)
	}
	frac, err := json.Marshal(ClaimSet{Expiry: c.Expiry})
	if err != nil {
		t.Fatal(err)
	}
	if string(frac) != `{"exp":1900000000.75}` {
		t.Errorf("fractional exp marshals as %s, want the fraction preserved", frac)
	}

	if err := json.Unmarshal([]byte(`{"exp":1.9e9}`), &c); err != nil {
		t.Fatalf("exponent-form input rejected: %v", err)
	}
	raw, err := json.Marshal(ClaimSet{Expiry: NewNumericDate(time.Unix(1900000000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"exp":1900000000}` {
		t.Errorf("whole exp marshals as %s, want plain integer seconds", raw)
	}

	// JSON decoding rounds 2^53+1 to 2^53; both are outside the accepted domain.
	rejects := []string{
		`{"exp":"soon"}`, `{"exp":5e18}`, `{"exp":-5e18}`,
		`{"exp":9007199254740993}`,
	}
	var direct NumericDate
	if err := json.Unmarshal([]byte(`null`), &direct); err == nil {
		t.Error("direct null accepted as a NumericDate")
	}
	for _, in := range rejects {
		var bad ClaimSet
		if err := json.Unmarshal([]byte(in), &bad); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
	var edge ClaimSet
	if err := json.Unmarshal([]byte(`{"exp":9007199254740991}`), &edge); err != nil {
		t.Errorf("last exact integer rejected: %v", err)
	}
	if _, err := json.Marshal(ClaimSet{Expiry: ptr(NumericDate(5e18))}); err == nil {
		t.Error("out-of-range constructed date marshaled")
	}

	var absent ClaimSet
	if err := json.Unmarshal([]byte(`{"exp":null}`), &absent); err != nil || absent.Expiry != nil {
		t.Errorf("exp:null = %v, %v; want absent", absent.Expiry, err)
	}
}

func TestScopes(t *testing.T) {
	tests := []struct {
		scope string
		want  []string
	}{
		{"", nil},
		{"   ", nil},
		{"read", []string{"read"}},
		{"read  write", []string{"read", "write"}},
	}
	for _, tt := range tests {
		c := ClaimSet{Scope: tt.scope}
		if got := c.Scopes(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Scopes(%q) = %#v, want %#v", tt.scope, got, tt.want)
		}
	}
}

func TestClaimSetClone(t *testing.T) {
	if (*ClaimSet)(nil).Clone() != nil {
		t.Fatal("nil Clone != nil")
	}
	orig := &ClaimSet{
		Subject:      "alice",
		Audience:     Audience{"api"},
		Expiry:       NewNumericDate(time.Unix(1900000000, 0)),
		NotBefore:    NewNumericDate(time.Unix(1800000000, 0)),
		IssuedAt:     NewNumericDate(time.Unix(1700000000, 0)),
		Roles:        []string{"admin"},
		Groups:       []string{"staff"},
		Entitlements: []string{"billing"},
	}
	c := orig.Clone()
	if !reflect.DeepEqual(orig, c) {
		t.Fatalf("clone differs:\n orig=%+v\nclone=%+v", orig, c)
	}
	c.Audience[0] = "evil"
	c.Roles[0] = "evil"
	c.Groups[0] = "evil"
	c.Entitlements[0] = "evil"
	*c.Expiry = NumericDate(1)
	*c.NotBefore = NumericDate(1)
	*c.IssuedAt = NumericDate(1)
	if orig.Audience[0] != "api" || orig.Roles[0] != "admin" ||
		orig.Groups[0] != "staff" || orig.Entitlements[0] != "billing" {
		t.Fatal("mutating the clone's slices reached the original")
	}
	if orig.Expiry.Time().Unix() != 1900000000 || orig.NotBefore.Time().Unix() != 1800000000 ||
		orig.IssuedAt.Time().Unix() != 1700000000 {
		t.Fatal("mutating the clone's dates reached the original")
	}
}
