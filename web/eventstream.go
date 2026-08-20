package web

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// defaultStreamWriteTimeout bounds one write, not the stream. Without it a
// client that stops reading without closing blocks the handler inside Flush,
// where the request context can no longer reach it - so it is a default rather
// than something a caller has to remember.
const defaultStreamWriteTimeout = 10 * time.Second

// EventStream responds with a server-sent event stream carrying every string
// events produces, until events closes or the client goes away.
//
//	return web.EventStream(lines).
//		Initial(recent...).
//		Heartbeat(20 * time.Second).
//		OnClose(unsubscribe), nil
//
// Admission is the caller's: decide whether to serve a stream at all, and
// answer something else if not, before returning one of these.
func EventStream(events <-chan string) EventStreamResponse {
	return EventStreamResponse{events: events, writeTimeout: defaultStreamWriteTimeout}
}

// EventStreamResponse is a stream of events. Chaining returns a copy.
type EventStreamResponse struct {
	events       <-chan string
	initial      []string
	heartbeat    time.Duration
	writeTimeout time.Duration
	onClose      func()
	headers      []headerPair
}

// Initial returns a copy that sends these events before any live one.
//
// They are sent, not replayed: this makes no promise that a client
// reconnecting will not see them again. What is worth resending, and whether
// a reconnecting client can tell, is the source's to decide.
func (v EventStreamResponse) Initial(events ...string) EventStreamResponse {
	// Cloned, so the caller's slice and this copy cannot change each other.
	v.initial = slices.Clone(events)
	return v
}

// Heartbeat returns a copy that sends a comment every d, keeping an idle
// connection from being reaped by anything between here and the browser.
// Zero, the default, sends none.
func (v EventStreamResponse) Heartbeat(d time.Duration) EventStreamResponse {
	v.heartbeat = d
	return v
}

// WriteTimeout returns a copy bounding each write. A duration that is not
// positive keeps the default; there is deliberately no way to remove it.
func (v EventStreamResponse) WriteTimeout(d time.Duration) EventStreamResponse {
	if d > 0 {
		v.writeTimeout = d
	}
	return v
}

// OnClose returns a copy that runs fn once the stream ends, however it ends.
func (v EventStreamResponse) OnClose(fn func()) EventStreamResponse {
	v.onClose = fn
	return v
}

// Header returns a copy carrying one more header.
func (v EventStreamResponse) Header(key, value string) EventStreamResponse {
	v.headers = withHeader(v.headers, key, value)
	return v
}

func (v EventStreamResponse) Respond(w http.ResponseWriter, r *http.Request) error {
	if v.onClose != nil {
		defer v.onClose()
	}

	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	applyHeaders(w, v.headers)
	w.WriteHeader(http.StatusOK)
	// The handshake is the headers arriving. A silent stream queues nothing,
	// so flush them now rather than with the first frame.
	if !v.send(rc, w, "") {
		return nil
	}

	if len(v.initial) > 0 {
		var frames strings.Builder
		for _, event := range v.initial {
			writeEvent(&frames, event)
		}
		if !v.send(rc, w, frames.String()) {
			return nil
		}
	}

	var beat <-chan time.Time
	if v.heartbeat > 0 {
		ticker := time.NewTicker(v.heartbeat)
		defer ticker.Stop()
		beat = ticker.C
	}

	for {
		select {
		case <-r.Context().Done():
			return nil

		case event, open := <-v.events:
			if !open {
				return nil
			}
			var frame strings.Builder
			writeEvent(&frame, event)
			if !v.send(rc, w, frame.String()) {
				return nil
			}

		case <-beat:
			if !v.send(rc, w, ":\n\n") {
				return nil
			}
		}
	}
}

// send writes one frame under a fresh deadline; an empty frame just flushes.
// A client going away is how a stream ends rather than a failure to report,
// so it reports whether to carry on rather than returning an error.
//
// A writer that cannot take a deadline or cannot flush is not a reason to hang
// up: the stream still delivers, just without those guarantees.
func (v EventStreamResponse) send(rc *http.ResponseController, w io.Writer, frame string) bool {
	if err := rc.SetWriteDeadline(time.Now().Add(v.writeTimeout)); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return false
	}
	if _, err := io.WriteString(w, frame); err != nil {
		return false
	}
	if err := rc.Flush(); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return false
	}
	return true
}

// writeEvent encodes one string as exactly one event, whatever line breaks it
// carries. Every break becomes another data line of the same event, so nothing
// in the data can end the event early or forge a field of its own.
func writeEvent(out *strings.Builder, data string) {
	data = strings.ReplaceAll(data, "\r\n", "\n")
	data = strings.ReplaceAll(data, "\r", "\n")

	for line := range strings.SplitSeq(data, "\n") {
		out.WriteString("data: ")
		out.WriteString(line)
		out.WriteString("\n")
	}
	out.WriteString("\n")
}
