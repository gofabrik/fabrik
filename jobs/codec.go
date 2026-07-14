package jobs

import (
	"encoding/json"
	"fmt"
)

// encodePayload serializes a message to its persisted JSON payload.
func encodePayload(msg any) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("jobs: nil message")
	}
	return json.Marshal(msg)
}

// decodePayload writes a persisted payload into a fresh message value.
// A decode failure wraps [ErrDecodePayload]; the runtime maps that to a
// terminal park.
func decodePayload(data []byte, into any) error {
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("%w: %v", ErrDecodePayload, err)
	}
	return nil
}
