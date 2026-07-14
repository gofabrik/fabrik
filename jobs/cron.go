package jobs

import "fmt"

// cronPrefix namespaces cron internal kinds.
const cronPrefix = "cron:"

// cronKind is the wire kind for a cron's internal job. Each cron gets a
// distinct kind so its schedule fires only its own function.
func cronKind(name string) string { return cronPrefix + name }

// RegisterCron binds a named function to an in-memory cron declaration.
// [Manager.ReconcileSchedules] persists declarations to the store.
//
// Each fire is a durable job with no message payload.
func RegisterCron(m *Manager, name, schedule string, fn func(Context) error) error {
	if m == nil {
		return fmt.Errorf("jobs.RegisterCron: nil manager")
	}
	if fn == nil {
		return fmt.Errorf("jobs.RegisterCron: nil function")
	}
	if err := validIdent("cron name", name); err != nil {
		return err
	}
	spec := Cron(schedule)
	if err := spec.validate(); err != nil {
		return err
	}
	kind := cronKind(name)

	// Declare before registering the handler, so duplicate names leave no
	// partial handler state.
	row, err := m.buildScheduleRow(name, kind, []byte("{}"), spec, ScheduleOptions{Singleton: true})
	if err != nil {
		return err
	}
	if err := m.declare(row); err != nil {
		return err
	}

	m.mu.Lock()
	m.handlers[HandlerKey{Kind: kind, HandlerID: name}] = handlerEntry{
		decode: func([]byte) (any, error) { return struct{}{}, nil },
		invoke: func(c Context, _ any) error { return fn(c) },
	}
	m.kindHandlers[kind] = []string{name}
	m.mu.Unlock()
	return nil
}
