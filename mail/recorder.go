package mail

import (
	"context"
	"sync"
)

// Recorder captures deep copies of deliveries for tests.
type Recorder struct {
	mu   sync.Mutex
	sent []Message
}

func (r *Recorder) Send(ctx context.Context, m *Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	snap := cloneMessage(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, snap)
	return nil
}

// Sent returns deep copies of the recorded messages in append order.
func (r *Recorder) Sent() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Message, len(r.sent))
	for i := range r.sent {
		out[i] = cloneMessage(&r.sent[i])
	}
	return out
}
