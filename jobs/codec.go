package jobs

import (
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"fmt"
)

func encodePayload(msg any) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("jobs: nil message")
	}
	return json.Marshal(msg, jsonv1.DefaultOptionsV1())
}

// decodePayload wraps malformed persisted data with [ErrDecodePayload].
func decodePayload(data []byte, into any) error {
	if err := json.Unmarshal(data, into, jsonv1.DefaultOptionsV1()); err != nil {
		return fmt.Errorf("%w: %v", ErrDecodePayload, err)
	}
	return nil
}
