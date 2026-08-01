package session

import (
	jsonv1 "encoding/json"
	"strings"
	"testing"
	"time"
)

// Duplicate cell names retain v1 last-wins decoding.
func TestEnvelopeDecodeKeepsV1Contracts(t *testing.T) {
	cells, err := decodeEnvelope([]byte(`{"cell":{"n":1},"cell":{"n":2}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(cells["cell"]) != `{"n":2}` {
		t.Fatalf("duplicate cell = %s, want last-wins", cells["cell"])
	}
}

// V1-escaped payloads round-trip through both codecs and remain readable by v1.
func TestV1WrittenPayloadRoundTrips(t *testing.T) {
	hostile := "a<b>&c line\u2028sep"
	cellV1, err := jsonv1.Marshal(compatCell{Name: hostile, Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cellV1), `\u003c`) || !strings.Contains(string(cellV1), `\u2028`) {
		t.Fatalf("fixture is not v1-escaped: %s", cellV1)
	}
	envV1, err := jsonv1.Marshal(map[string]jsonv1.RawMessage{"c": cellV1})
	if err != nil {
		t.Fatal(err)
	}

	cells, err := decodeEnvelope(envV1)
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	h := &Handle[compatCell]{key: "c"}
	v, err := h.decode(cells["c"])
	if err != nil {
		t.Fatalf("decode cell: %v", err)
	}
	if v.Name != hostile || v.Wait != time.Second {
		t.Fatalf("decoded = %+v, want the original value", v)
	}

	reRaw, err := h.encode(v)
	if err != nil {
		t.Fatalf("re-encode cell: %v", err)
	}
	reEnv, err := encodeEnvelope(map[string]cellRaw{"c": reRaw})
	if err != nil {
		t.Fatalf("re-encode envelope: %v", err)
	}
	var back map[string]jsonv1.RawMessage
	if err := jsonv1.Unmarshal(reEnv, &back); err != nil {
		t.Fatalf("v1 cannot read the re-encoded envelope: %v", err)
	}
	var v2 compatCell
	if err := jsonv1.Unmarshal(back["c"], &v2); err != nil {
		t.Fatalf("v1 cannot read the re-encoded cell: %v", err)
	}
	if v2 != v {
		t.Fatalf("round-trip drifted: %+v vs %+v", v2, v)
	}
}

// Duplicate members retain v1 last-wins decoding and re-encode without duplicates.
func TestDuplicateMemberCellRoundTrips(t *testing.T) {
	cells, err := decodeEnvelope([]byte(`{"c":{"name":"first","name":"second","wait":1000000000}}`))
	if err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	h := &Handle[compatCell]{key: "c"}
	v, err := h.decode(cells["c"])
	if err != nil {
		t.Fatalf("decode cell: %v", err)
	}
	if v.Name != "second" || v.Wait != time.Second {
		t.Fatalf("decoded = %+v, want last-wins name and duration", v)
	}
	reRaw, err := h.encode(v)
	if err != nil {
		t.Fatalf("re-encode cell: %v", err)
	}
	reEnv, err := encodeEnvelope(map[string]cellRaw{"c": reRaw})
	if err != nil {
		t.Fatalf("re-encode envelope: %v", err)
	}
	var back map[string]jsonv1.RawMessage
	if err := jsonv1.Unmarshal(reEnv, &back); err != nil {
		t.Fatalf("v1 cannot read the re-encoded envelope: %v", err)
	}
	var v2 compatCell
	if err := jsonv1.Unmarshal(back["c"], &v2); err != nil {
		t.Fatalf("v1 cannot read the re-encoded cell: %v", err)
	}
	if v2 != v {
		t.Fatalf("round-trip drifted: %+v vs %+v", v2, v)
	}
}

func TestEnvelopeEncodeSortsCellKeys(t *testing.T) {
	out, err := encodeEnvelope(map[string]cellRaw{
		"zeta":  []byte(`1`),
		"alpha": []byte(`2`),
		"mid":   []byte(`3`),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	s := string(out)
	if !(strings.Index(s, `"alpha"`) < strings.Index(s, `"mid"`) && strings.Index(s, `"mid"`) < strings.Index(s, `"zeta"`)) {
		t.Fatalf("cell keys not sorted: %s", s)
	}
}

type compatCell struct {
	Name string        `json:"name"`
	Wait time.Duration `json:"wait"`
	Flag bool          `json:"flag,omitempty"`
}

// Typed cells preserve v1 field matching, duration, and omitempty semantics.
func TestHandleCodecKeepsV1Semantics(t *testing.T) {
	h := &Handle[compatCell]{key: "c"}
	v, err := h.decode([]byte(`{"NAME":"x","wait":5000000000}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Name != "x" || v.Wait != 5*time.Second {
		t.Fatalf("decoded = %+v, want case-insensitive match and nanosecond duration", v)
	}
	raw, err := h.encode(compatCell{Name: "y", Wait: time.Second})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"name":"y","wait":1000000000}`
	if string(raw) != want {
		t.Fatalf("encoded = %s, want %s", raw, want)
	}
}
