package jobs

import (
	"encoding/json"
	"math/rand/v2"
	"time"
)

// Backoff computes the delay before a job's next attempt. Implementations
// must be safe for concurrent use.
type Backoff interface {
	Next(attempt int) time.Duration
}

// ExponentialBackoff doubles the delay each attempt, clamped to Max,
// with symmetric jitter applied last. It is the one backoff that
// round-trips as a per-job override; other implementations are usable
// only as [Config.DefaultBackoff].
type ExponentialBackoff struct {
	Base   time.Duration
	Max    time.Duration
	Jitter float64 // fraction, e.g. 0.2 for +/-20%
}

// DefaultBackoff is used when neither the per-job override nor
// [Config.DefaultBackoff] is set: 1s base, 1h cap, 20% jitter.
var DefaultBackoff Backoff = ExponentialBackoff{
	Base:   1 * time.Second,
	Max:    1 * time.Hour,
	Jitter: 0.2,
}

// Next returns Base * 2^(attempt-1), clamped to Max, jittered. attempt
// below 1 is treated as 1.
func (b ExponentialBackoff) Next(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := b.Max
	if shift := uint(attempt - 1); shift < 63 {
		if cand := b.Base << shift; cand > 0 && cand < b.Max {
			d = cand
		}
	}
	if b.Jitter > 0 && d > 0 {
		if span := time.Duration(float64(d) * b.Jitter); span > 0 {
			d = d - span + time.Duration(rand.Int64N(int64(2*span)+1))
			if d < 0 {
				d = 0
			}
		}
	}
	return d
}

type backoffSpec struct {
	Type   string        `json:"type"`
	Base   time.Duration `json:"base,omitempty"`
	Max    time.Duration `json:"max,omitempty"`
	Jitter float64       `json:"jitter,omitempty"`
}

// encodeBackoff serializes per-job backoff overrides; custom backoffs are
// rejected because they cannot round-trip through persistence.
func encodeBackoff(b Backoff) ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	var exp ExponentialBackoff
	switch v := b.(type) {
	case ExponentialBackoff:
		exp = v
	case *ExponentialBackoff:
		if v == nil {
			return nil, nil
		}
		exp = *v
	default:
		return nil, ErrBackoffNotSerializable
	}
	return json.Marshal(backoffSpec{Type: "exponential", Base: exp.Base, Max: exp.Max, Jitter: exp.Jitter})
}

// decodeBackoff reconstructs a backoff from its spec, or nil for empty
// input or an unrecognized type (callers fall back to the default).
func decodeBackoff(data []byte) Backoff {
	if len(data) == 0 {
		return nil
	}
	var spec backoffSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil
	}
	if spec.Type == "exponential" {
		return ExponentialBackoff{Base: spec.Base, Max: spec.Max, Jitter: spec.Jitter}
	}
	return nil
}
