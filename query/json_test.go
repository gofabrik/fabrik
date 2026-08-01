package query

import (
	jsonv1 "encoding/json"
	"testing"
	"time"
)

type jsonColumnPayload struct {
	Name string        `json:"name"`
	Wait time.Duration `json:"wait"`
	Flag bool          `json:"flag,omitempty"`
}

// Stored column bytes preserve v1 duration and omitempty encoding.
func TestJSONValueMatchesV1Bytes(t *testing.T) {
	j := JSON[jsonColumnPayload]{V: jsonColumnPayload{Name: "x", Wait: 2 * time.Second}}
	got, err := j.Value()
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonv1.Marshal(j.V)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("Value() = %s, want v1 bytes %s", got, want)
	}
}

func TestJSONScanRoundTripsV1Bytes(t *testing.T) {
	data, err := jsonv1.Marshal(jsonColumnPayload{Name: "y", Wait: 5 * time.Minute, Flag: true})
	if err != nil {
		t.Fatal(err)
	}
	var j JSON[jsonColumnPayload]
	if err := j.Scan(data); err != nil {
		t.Fatal(err)
	}
	if j.V.Name != "y" || j.V.Wait != 5*time.Minute || !j.V.Flag {
		t.Fatalf("Scan = %+v", j.V)
	}
}
