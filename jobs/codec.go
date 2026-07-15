package jobs

import (
	"encoding/json"
	"fmt"
)

func encodePayload(msg any) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("jobs: nil message")
	}
	return json.Marshal(msg)
}

// decodePayload wraps malformed persisted data with [ErrDecodePayload].
func decodePayload(data []byte, into any) error {
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("%w: %v", ErrDecodePayload, err)
	}
	return nil
}
