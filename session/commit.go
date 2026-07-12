package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// commit finalizes a request's session state at response start.
func (m *core) commit(ctx context.Context, st *state, w http.ResponseWriter) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	// A commit failure still starts the response.
	st.responseStarted = true

	// Logout deletes first; a surviving authenticated SID is the
	// failure mode this ordering prevents.
	clearNeeded := st.staleToken
	if st.destroyed && st.destroyedSID != "" {
		if err := m.cfg.Store.Delete(ctx, st.destroyedSID); err != nil {
			return err
		}
		st.destroyed = false
		st.destroyedSID = ""
		clearNeeded = true
	}

	tokenWrite := false
	switch {
	case st.exists && st.renew:
		if err := m.rotateAtCommit(ctx, st); err != nil {
			return err
		}
		tokenWrite = true
	case st.exists && len(st.staged) > 0:
		if err := m.commitDirty(ctx, st); err != nil {
			return err
		}
		tokenWrite = true
	case !st.exists && st.pendingMint():
		if err := m.mintAtCommit(ctx, st); err != nil {
			return err
		}
		tokenWrite = true
	case st.exists:
		// Re-emit the token only when the server-side expiry actually
		// moved.
		if m.maybeBumpIdle(ctx, st) {
			tokenWrite = true
		}
		if st.tokenNeeded {
			tokenWrite = true
		}
	}

	// Set supersedes clear, so each commit emits one token instruction.
	switch {
	case tokenWrite:
		m.cfg.Token.Write(w, st.record.SID, TokenWriteOptions{Expiry: tokenExpiry(st.record), Now: m.now()})
	case clearNeeded:
		m.cfg.Token.Clear(w)
	}
	return nil
}

// rotateAtCommit writes the new SID before deleting the old row.
// Rotation never extends absolute expiry. Callers hold st.mu.
func (m *core) rotateAtCommit(ctx context.Context, st *state) error {
	// Rotation preserves malformed payload bytes when no staged cells
	// require decoding.
	var payload []byte
	var cells map[string]cellRaw
	if err := st.decodeCells(); err != nil {
		payload = st.record.Payload
	} else {
		cells = st.mergedView()
		var eerr error
		payload, eerr = encodeEnvelope(cells)
		if eerr != nil {
			return eerr
		}
	}
	oldSID := st.record.SID
	rec := Record{
		UserID:         st.record.UserID,
		AbsoluteExpiry: st.record.AbsoluteExpiry,
		Payload:        payload,
	}
	if st.promote {
		rec.UserID = st.promotedID
	}
	if m.cfg.IdleExpiry > 0 {
		rec.IdleExpiry = m.now().Add(m.cfg.IdleExpiry)
	}
	stored, err := m.insertFresh(ctx, rec)
	if err != nil {
		return err
	}
	if err := m.cfg.Store.Delete(ctx, oldSID); err != nil {
		if st.promote && st.record.UserID != "" {
			// The old session was itself authenticated and we are
			// rotating it (a re-login or privilege change): its SID
			// must not silently outlive the rotation on a stolen
			// token. The commit fails, no token is issued, and the
			// residue is recoverable storage - the new row sits
			// unreferenced (its SID never left the process) until it
			// expires, while the client keeps the prior session and
			// sees the error.
			return err
		}
		// A fresh login promotes an anonymous session (old UserID
		// empty), and renew carries the same identity: in both a
		// surviving old row is harmless - it holds no authenticated
		// identity that was just superseded, and expires on its own.
		// Best-effort.
	}
	st.record = stored
	if cells != nil {
		st.cells = cells
	}
	st.staged = nil
	return nil
}

// commitDirty overlays staged cells on the loaded cell map and
// CAS-saves, reloading on conflicts. Callers hold st.mu.
func (m *core) commitDirty(ctx context.Context, st *state) error {
	if err := st.decodeCells(); err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		cells := st.mergedView()
		payload, err := encodeEnvelope(cells)
		if err != nil {
			return err
		}
		rec := st.record
		rec.Payload = payload
		if m.cfg.IdleExpiry > 0 {
			rec.IdleExpiry = m.now().Add(m.cfg.IdleExpiry)
		}
		stored, err := m.cfg.Store.Save(ctx, rec)
		if err == nil {
			st.record = stored
			st.cells = cells
			st.staged = nil
			return nil
		}
		if !errors.Is(err, ErrVersionConflict) || attempt >= m.maxRetries {
			return err
		}
		fresh, lerr := m.cfg.Store.Load(ctx, st.record.SID)
		if lerr != nil {
			return lerr
		}
		freshCells, derr := decodeEnvelope(freshPayload(fresh))
		if derr != nil {
			return derr
		}
		st.record = fresh
		st.cells = freshCells
	}
}

// mintAtCommit creates the new session a sessionless request staged.
// Callers hold st.mu.
func (m *core) mintAtCommit(ctx context.Context, st *state) error {
	cells := st.mergedView()
	payload, err := encodeEnvelope(cells)
	if err != nil {
		return err
	}
	now := m.now()
	rec := Record{
		AbsoluteExpiry: now.Add(m.cfg.AbsoluteExpiry),
		Payload:        payload,
	}
	if st.promote {
		rec.UserID = st.promotedID
	}
	if m.cfg.IdleExpiry > 0 {
		rec.IdleExpiry = now.Add(m.cfg.IdleExpiry)
	}
	stored, err := m.insertFresh(ctx, rec)
	if err != nil {
		return err
	}
	st.record = stored
	st.exists = true
	st.cells = cells
	st.cellsDecoded = true
	st.staged = nil
	return nil
}

// insertFresh retries SID collisions without merging into another
// session's record.
func (m *core) insertFresh(ctx context.Context, rec Record) (Record, error) {
	for attempt := 0; ; attempt++ {
		sid, err := m.mintSID()
		if err != nil {
			return Record{}, err
		}
		rec.SID = sid
		rec.Version = 0
		stored, err := m.cfg.Store.Save(ctx, rec)
		if err == nil {
			return stored, nil
		}
		if !errors.Is(err, ErrVersionConflict) || attempt >= m.maxRetries {
			return Record{}, fmt.Errorf("session: mint: %w", err)
		}
	}
}

// maybeBumpIdle extends a clean-read session's idle expiry through
// TTLBumper. Returns whether the bump ran. Callers hold st.mu.
func (m *core) maybeBumpIdle(ctx context.Context, st *state) bool {
	if m.cfg.IdleBumpInterval <= 0 || m.cfg.IdleExpiry <= 0 {
		return false
	}
	bumper, ok := m.cfg.Store.(TTLBumper)
	if !ok {
		return false
	}
	now := m.now()
	// Bump only after IdleBumpInterval has elapsed since the last bump.
	remaining := st.record.IdleExpiry.Sub(now)
	if remaining > m.cfg.IdleExpiry-m.cfg.IdleBumpInterval {
		return false
	}
	newExpiry := now.Add(m.cfg.IdleExpiry)
	if err := bumper.BumpTTL(ctx, st.record.SID, newExpiry); err != nil {
		return false
	}
	st.record.IdleExpiry = newExpiry
	return true
}

// tokenExpiry returns the earlier enabled expiry.
func tokenExpiry(rec Record) time.Time {
	a, i := rec.AbsoluteExpiry, rec.IdleExpiry
	switch {
	case a.IsZero():
		return i
	case i.IsZero():
		return a
	case i.Before(a):
		return i
	default:
		return a
	}
}
