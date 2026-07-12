package session

import (
	"encoding/json"
	"fmt"
)

// decodeEnvelope parses payload bytes into the cell map.
func decodeEnvelope(payload []byte) (map[string]cellRaw, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
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
	raw := make(map[string]json.RawMessage, len(cells))
	for k, v := range cells {
		raw[k] = json.RawMessage(v)
	}
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("session: encode payload envelope: %w", err)
	}
	return out, nil
}

// mergedView returns stored cells overlaid with staged writes.
func (st *state) mergedView() map[string]cellRaw {
	out := make(map[string]cellRaw, len(st.cells)+len(st.staged))
	for k, v := range st.cells {
		out[k] = v
	}
	for k, v := range st.staged {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}
