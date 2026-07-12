package session

import (
	"context"
	"errors"
	"fmt"
)

// checkStageable reports ErrAlreadyCommitted before data validation.
func (m *core) checkStageable(ctx context.Context, op string) error {
	st, err := m.stateFromCtx(ctx, op)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.responseStarted {
		return fmt.Errorf("session.%s: %w", op, ErrAlreadyCommitted)
	}
	return nil
}

// cellGet returns the request-current raw value of one cell.
func (m *core) cellGet(ctx context.Context, key string) (cellRaw, bool, error) {
	st, err := m.stateFromCtx(ctx, "Get")
	if err != nil {
		return nil, false, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := m.ensureLoaded(ctx, st); err != nil {
		return nil, false, err
	}
	if v, ok := st.staged[key]; ok {
		if v == nil {
			return nil, false, nil
		}
		return v, true, nil
	}
	if err := st.decodeCells(); err != nil {
		return nil, false, err
	}
	v, ok := st.cells[key]
	return v, ok, nil
}

// cellHas reports request-current existence.
func (m *core) cellHas(ctx context.Context, key string) (bool, error) {
	st, err := m.stateFromCtx(ctx, "Has")
	if err != nil {
		return false, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := m.ensureLoaded(ctx, st); err != nil {
		return false, err
	}
	if v, ok := st.staged[key]; ok {
		return v != nil, nil
	}
	if err := st.decodeCells(); err != nil {
		return false, err
	}
	_, ok := st.cells[key]
	return ok, nil
}

// cellSave stages one whole-cell overwrite without decoding old cell bytes.
func (m *core) cellSave(ctx context.Context, key string, raw cellRaw) error {
	st, err := m.stateFromCtx(ctx, "Save")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.responseStarted {
		return fmt.Errorf("session.Save: %w", ErrAlreadyCommitted)
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	if st.exists {
		if err := st.decodeCells(); err != nil {
			return err
		}
	}
	st.stage(key, raw)
	return nil
}

// cellClear stages removal of one cell and never mints a session.
func (m *core) cellClear(ctx context.Context, key string) error {
	st, err := m.stateFromCtx(ctx, "Clear")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.responseStarted {
		return fmt.Errorf("session.Clear: %w", ErrAlreadyCommitted)
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	if v, ok := st.staged[key]; ok {
		if v != nil {
			st.stage(key, nil)
		}
		return nil
	}
	if !st.exists {
		return nil
	}
	if err := st.decodeCells(); err != nil {
		return err
	}
	if _, ok := st.cells[key]; ok {
		st.stage(key, nil)
	}
	return nil
}

// stage records one staged write and bumps the cell's sequence.
func (st *state) stage(key string, raw cellRaw) {
	if st.staged == nil {
		st.staged = map[string][]byte{}
		st.stagedSeq = map[string]int{}
	}
	st.staged[key] = raw
	st.stagedSeq[key]++
}

// cellUpdate is the immediate read-modify-write path. The closure
// runs outside st.mu; store CAS serializes concurrent updates.
func (m *core) cellUpdate(ctx context.Context, key string, fn func(prev cellRaw) (cellRaw, error)) error {
	st, err := m.stateFromCtx(ctx, "Update")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	if !st.exists && st.responseStarted {
		// A minting Update after response start cannot send its token.
		return fmt.Errorf("session.Update: %w", ErrAlreadyCommitted)
	}
	if st.exists {
		if err := st.decodeCells(); err != nil {
			return err
		}
	}

	for attempt := 0; ; attempt++ {
		base, stagedBase, seq, err := st.updateBase(key)
		if err != nil {
			return err
		}

		// Re-lock during unwind to keep st.mu balanced.
		var out cellRaw
		var ferr error
		st.mu.Unlock()
		func() {
			defer st.mu.Lock()
			out, ferr = fn(base)
		}()
		if ferr != nil {
			return ferr
		}

		if !st.exists {
			// SID-collision retries do not re-run the closure.
			return m.mintForUpdate(ctx, st, key, out)
		}

		merged := make(map[string]cellRaw, len(st.cells)+1)
		for k, v := range st.cells {
			merged[k] = v
		}
		merged[key] = out
		payload, err := encodeEnvelope(merged)
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
			st.cells = merged
			st.tokenNeeded = true
			if stagedBase && st.stagedSeq[key] == seq {
				// Save during the closure re-dirties the cell and wins.
				delete(st.staged, key)
			}
			return nil
		}
		if !errors.Is(err, ErrVersionConflict) || attempt >= m.maxRetries {
			return err
		}
		if err := m.reloadCells(ctx, st); err != nil {
			return err
		}
	}
}

// reloadCells refreshes state after a CAS conflict. Callers hold st.mu.
func (m *core) reloadCells(ctx context.Context, st *state) error {
	fresh, err := m.cfg.Store.Load(ctx, st.record.SID)
	if err != nil {
		return err
	}
	cells, err := decodeEnvelope(freshPayload(fresh))
	if err != nil {
		return err
	}
	st.record = fresh
	st.cells = cells
	return nil
}

// updateBase computes one attempt's closure input under st.mu.
func (st *state) updateBase(key string) (base cellRaw, stagedBase bool, seq int, err error) {
	if v, ok := st.staged[key]; ok {
		return v, true, st.stagedSeq[key], nil
	}
	if st.exists {
		if err := st.decodeCells(); err != nil {
			return nil, false, 0, err
		}
		return st.cells[key], false, 0, nil
	}
	return nil, false, 0, nil
}

// mintForUpdate creates the session a pre-commit Update needs now.
// Callers hold st.mu.
func (m *core) mintForUpdate(ctx context.Context, st *state, key string, out cellRaw) error {
	if st.destroyed && st.destroyedSID != "" {
		if err := m.cfg.Store.Delete(ctx, st.destroyedSID); err != nil {
			return err
		}
		st.destroyed = false
		st.destroyedSID = ""
	}

	cells := st.mergedView()
	cells[key] = out
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
	st.tokenNeeded = true
	// The mint consumed the staged view held under lock.
	st.staged = nil
	return nil
}

// freshPayload treats nil or empty payloads as an empty envelope.
func freshPayload(rec Record) []byte {
	if len(rec.Payload) == 0 {
		return []byte("{}")
	}
	return rec.Payload
}

// loadCell reads one cell out of band without bumping idle TTL.
func (m *core) loadCell(ctx context.Context, sid, key string) (cellRaw, bool, error) {
	rec, err := m.cfg.Store.Load(ctx, sid)
	if err != nil {
		return nil, false, err
	}
	cells, err := decodeEnvelope(freshPayload(rec))
	if err != nil {
		return nil, false, err
	}
	v, ok := cells[key]
	return v, ok, nil
}

// mutateCellsSID is the out-of-band CAS loop shared by update and clear.
func (m *core) mutateCellsSID(ctx context.Context, sid string, mutate func(cells map[string]cellRaw) (bool, error)) error {
	for attempt := 0; ; attempt++ {
		rec, err := m.cfg.Store.Load(ctx, sid)
		if err != nil {
			return err
		}
		cells, err := decodeEnvelope(freshPayload(rec))
		if err != nil {
			return err
		}
		write, err := mutate(cells)
		if err != nil {
			return err
		}
		if !write {
			return nil
		}
		payload, err := encodeEnvelope(cells)
		if err != nil {
			return err
		}
		rec.Payload = payload
		if _, err := m.cfg.Store.Save(ctx, rec); err != nil {
			if errors.Is(err, ErrVersionConflict) && attempt < m.maxRetries {
				continue
			}
			return err
		}
		return nil
	}
}

// updateCellSID updates one cell out of band without minting.
func (m *core) updateCellSID(ctx context.Context, sid, key string, fn func(prev cellRaw) (cellRaw, error)) error {
	return m.mutateCellsSID(ctx, sid, func(cells map[string]cellRaw) (bool, error) {
		out, err := fn(cells[key])
		if err != nil {
			return false, err
		}
		cells[key] = out
		return true, nil
	})
}

// clearCellSID removes one cell out of band.
func (m *core) clearCellSID(ctx context.Context, sid, key string) error {
	return m.mutateCellsSID(ctx, sid, func(cells map[string]cellRaw) (bool, error) {
		if _, ok := cells[key]; !ok {
			return false, nil
		}
		delete(cells, key)
		return true, nil
	})
}
