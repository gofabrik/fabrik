package session

import (
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"maps"
)

// decodeEnvelope parses payload bytes into the cell map.
func decodeEnvelope(payload []byte) (map[string]cellRaw, error) {
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(payload, &raw, jsonv1.DefaultOptionsV1()); err != nil {
		return nil, fmt.Errorf("session: malformed payload envelope: %w", err)
	}
	cells := make(map[string]cellRaw, len(raw))
	for k, v := range raw {
		cells[k] = []byte(v)
	}
	return cells, nil
}

// encodeEnvelope renders the cell map as one JSON object.
func encodeEnvelope(cells map[string]cellRaw) ([]byte, error) {
	if len(cells) == 0 {
		return []byte("{}"), nil
	}
	raw := make(map[string]jsontext.Value, len(cells))
	for k, v := range cells {
		raw[k] = jsontext.Value(v)
	}
	out, err := json.Marshal(raw, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("session: encode payload envelope: %w", err)
	}
	return out, nil
}

// mergedView returns stored cells overlaid with staged writes.
func (st *state) mergedView() map[string]cellRaw {
	out := make(map[string]cellRaw, len(st.cells)+len(st.staged))
	maps.Copy(out, st.cells)
	for k, v := range st.staged {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}
