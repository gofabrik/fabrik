package jobs

import (
	jsonv1 "encoding/json"
	"testing"
	"time"
)

type codecPayload struct {
	Name string        `json:"name"`
	Wait time.Duration `json:"wait"`
	Flag bool          `json:"flag,omitempty"`
}

// Durable payload bytes preserve v1 duration and omitempty encoding.
func TestEncodePayloadMatchesV1Bytes(t *testing.T) {
	p := codecPayload{Name: "x", Wait: 2 * time.Second}
	got, err := encodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonv1.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("encodePayload = %s, want v1 bytes %s", got, want)
	}
}

func TestDecodePayloadRoundTrips(t *testing.T) {
	data, err := jsonv1.Marshal(codecPayload{Name: "y", Wait: 5 * time.Minute, Flag: true})
	if err != nil {
		t.Fatal(err)
	}
	var got codecPayload
	if err := decodePayload(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "y" || got.Wait != 5*time.Minute || !got.Flag {
		t.Fatalf("decodePayload = %+v", got)
	}
}
