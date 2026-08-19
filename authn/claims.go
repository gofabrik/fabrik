// Package authn defines claims, request-context transport, predicates,
// and route guards shared by authentication schemes.
package authn

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ClaimSet contains registered identity and authorization claims.
// Unknown JSON claims are discarded during unmarshal.
type ClaimSet struct {
	Issuer    string       `json:"iss,omitempty"`
	Subject   string       `json:"sub,omitempty"`
	Audience  Audience     `json:"aud,omitempty"`
	Expiry    *NumericDate `json:"exp,omitempty"`
	NotBefore *NumericDate `json:"nbf,omitempty"`
	IssuedAt  *NumericDate `json:"iat,omitempty"`
	ID        string       `json:"jti,omitempty"`

	Scope        string   `json:"scope,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	Roles        []string `json:"roles,omitempty"`
	Groups       []string `json:"groups,omitempty"`
	Entitlements []string `json:"entitlements,omitempty"`
}

// Scopes splits the space-delimited scope claim; nil when empty.
func (c *ClaimSet) Scopes() []string {
	fields := strings.Fields(c.Scope)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// Audience is the aud claim. It accepts a string or an array and
// marshals a single value as a string.
type Audience []string

func (a Audience) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	return json.Marshal([]string(a))
}

func (a *Audience) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return fmt.Errorf("authn: aud is neither a string nor an array of strings: null")
	}
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = Audience{single}
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("authn: aud is neither a string nor an array of strings: %s", data)
	}
	many := make([]string, len(raw))
	for i, el := range raw {
		if err := json.Unmarshal(el, &many[i]); err != nil || string(el) == "null" {
			return fmt.Errorf("authn: aud member is not a string: %s", el)
		}
	}
	*a = Audience(many)
	return nil
}

// NumericDate is an RFC 7519 timestamp in seconds since the Unix epoch.
// It preserves fractions, marshals whole values as integers, and rejects
// magnitudes outside float64's exact-integer domain.
type NumericDate float64

// numericDateLimit ensures accepted values round-trip through float64.
const numericDateLimit = float64(1 << 53)

// NewNumericDate returns the date for t, truncated to whole seconds.
func NewNumericDate(t time.Time) *NumericDate {
	d := NumericDate(t.Unix())
	return &d
}

// Time returns the date as a time.Time in UTC.
func (d *NumericDate) Time() time.Time {
	if d == nil {
		return time.Time{}
	}
	sec, frac := math.Modf(float64(*d))
	return time.Unix(int64(sec), int64(frac*float64(time.Second))).UTC()
}

func (d NumericDate) MarshalJSON() ([]byte, error) {
	f := float64(d)
	if math.IsNaN(f) || math.Abs(f) >= numericDateLimit {
		return nil, fmt.Errorf("authn: NumericDate out of range: %v", f)
	}
	if f == math.Trunc(f) {
		return strconv.AppendInt(nil, int64(f), 10), nil
	}
	return strconv.AppendFloat(nil, f, 'f', -1, 64), nil
}

func (d *NumericDate) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return fmt.Errorf("authn: NumericDate is not a number: null")
	}
	var f float64
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("authn: NumericDate is not a number: %s", data)
	}
	if math.IsNaN(f) || math.Abs(f) >= numericDateLimit {
		return fmt.Errorf("authn: NumericDate out of range: %s", data)
	}
	*d = NumericDate(f)
	return nil
}
