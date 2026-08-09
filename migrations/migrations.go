// Package migrations applies forward-only SQL migrations to a *sql.DB.
//
// The backend is supplied as a Driver value:
//
//	migrations.Migrate(ctx, db, sqlite.Driver(), src)
//
// See the sqlite, postgres and mysql leaf packages for Driver
// implementations.
package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrDrift is wrapped when an applied migration's stored checksum no
	// longer matches the file body in source.
	ErrDrift = errors.New("migration drift")

	// ErrOrphan is wrapped when schema_migrations records a migration
	// that no longer exists in source.
	ErrOrphan = errors.New("orphan migration")

	// ErrInvalidFilename is wrapped when a .sql file in source does not
	// match the NNNN_name.sql pattern.
	ErrInvalidFilename = errors.New("invalid migration filename")

	// ErrDuplicateVersion is wrapped when two files in one stream share
	// the same numeric version prefix.
	ErrDuplicateVersion = errors.New("duplicate migration version")

	// ErrDuplicateStream is wrapped when two sources name one stream.
	ErrDuplicateStream = errors.New("duplicate migration stream")

	// ErrInvalidSource is wrapped when a Source is malformed: nil FS,
	// or an invalid Dir or Stream.
	ErrInvalidSource = errors.New("invalid migration source")
)

type State int

const (
	StatePending State = iota
	StateApplied
	StateDrifted
	StateOrphan
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateApplied:
		return "applied"
	case StateDrifted:
		return "drifted"
	case StateOrphan:
		return "orphan"
	}
	return "unknown"
}

// MigrationStatus is one migration's source and database state.
type MigrationStatus struct {
	Stream  string
	Version int64
	Name    string
	// Checksum is empty for StatePending.
	Checksum string
	// FileChecksum is empty for StateOrphan.
	FileChecksum string
	AppliedAt    time.Time
	State        State
}

// Source is one migration stream.
type Source struct {
	// Stream names the stream. Empty is a valid stream key.
	Stream string
	FS     fs.FS
	// Dir is the subdirectory inside FS. Empty uses the whole FS.
	Dir string
}

// Sources is the migration configuration for one database.
type Sources []Source

// Migrate applies one root migration stream to db.
func Migrate(ctx context.Context, db *sql.DB, drv Driver, source fs.FS) error {
	return Sources{{FS: source}}.Migrate(ctx, db, drv)
}

// Status reports one root migration stream without modifying the database.
func Status(ctx context.Context, db *sql.DB, drv Driver, source fs.FS) ([]MigrationStatus, error) {
	return Sources{{FS: source}}.Status(ctx, db, drv)
}

// Check validates source shape without touching a database.
func (s Sources) Check() error {
	_, err := loadStreams(s)
	return err
}

// Migrate validates drift and orphans, then applies streams and versions in
// sorted order. Each migration commits independently, and reruns are idempotent.
func (s Sources) Migrate(ctx context.Context, db *sql.DB, drv Driver) (rerr error) {
	if drv == nil {
		return errors.New("migrations: nil Driver")
	}
	streams, err := loadStreams(s)
	if err != nil {
		return err
	}

	sess, err := drv.OpenSession(ctx, db)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil && rerr == nil {
			rerr = cerr
		}
	}()

	if _, err := sess.ExecContext(ctx, drv.SchemaSQL()); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := loadApplied(ctx, sess)
	if err != nil {
		return err
	}

	inSource := make(map[appliedKey]Migration)
	for _, st := range streams {
		for _, m := range st.migs {
			inSource[appliedKey{stream: st.name, version: m.Version}] = m
		}
	}
	for _, k := range sortedKeys(applied) {
		row := applied[k]
		m, ok := inSource[k]
		if !ok {
			return fmt.Errorf("migration %s is recorded as applied but missing from source: %w",
				displayName(k.stream, k.version, row.name), ErrOrphan)
		}
		if row.checksum != m.Checksum {
			return fmt.Errorf("migration %s has changed since it was applied (file checksum %s, stored %s): %w",
				displayName(k.stream, k.version, m.Name), m.Checksum, row.checksum, ErrDrift)
		}
	}

	insertSQL := "INSERT INTO schema_migrations (stream, version, name, checksum, applied_at) VALUES (" + placeholders(drv, 5) + ")"

	for _, st := range streams {
		for _, m := range st.migs {
			if _, ok := applied[appliedKey{stream: st.name, version: m.Version}]; ok {
				continue
			}
			if err := sess.Apply(ctx, st.name, m, insertSQL); err != nil {
				return fmt.Errorf("apply migration %s: %w", displayName(st.name, m.Version, m.Name), err)
			}
		}
	}
	return nil
}

// Status reports source and database rows, sorted by (Stream, Version).
// It is read-only and does not lock; concurrent Migrate calls may make
// the snapshot transient.
func (s Sources) Status(ctx context.Context, db *sql.DB, drv Driver) ([]MigrationStatus, error) {
	if drv == nil {
		return nil, errors.New("migrations: nil Driver")
	}
	streams, err := loadStreams(s)
	if err != nil {
		return nil, err
	}

	applied := map[appliedKey]appliedRow{}
	exists, err := drv.TableExists(ctx, db)
	if err != nil {
		return nil, err
	}
	if exists {
		applied, err = loadApplied(ctx, db)
		if err != nil {
			return nil, err
		}
	}

	var out []MigrationStatus
	inSource := make(map[appliedKey]bool)
	for _, st := range streams {
		for _, m := range st.migs {
			k := appliedKey{stream: st.name, version: m.Version}
			inSource[k] = true
			row, ok := applied[k]
			if !ok {
				out = append(out, MigrationStatus{
					Stream:       st.name,
					Version:      m.Version,
					Name:         m.Name,
					FileChecksum: m.Checksum,
					State:        StatePending,
				})
				continue
			}
			state := StateApplied
			if row.checksum != m.Checksum {
				state = StateDrifted
			}
			out = append(out, MigrationStatus{
				Stream:       st.name,
				Version:      m.Version,
				Name:         m.Name,
				Checksum:     row.checksum,
				FileChecksum: m.Checksum,
				AppliedAt:    row.appliedAt,
				State:        state,
			})
		}
	}
	for k, row := range applied {
		if inSource[k] {
			continue
		}
		out = append(out, MigrationStatus{
			Stream:    k.stream,
			Version:   k.version,
			Name:      row.name,
			Checksum:  row.checksum,
			AppliedAt: row.appliedAt,
			State:     StateOrphan,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stream != out[j].Stream {
			return out[i].Stream < out[j].Stream
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

type appliedRow struct {
	name      string
	checksum  string
	appliedAt time.Time
}

type appliedKey struct {
	stream  string
	version int64
}

type stream struct {
	name string
	migs []Migration
}

var filenameRE = regexp.MustCompile(`^(\d+)_([A-Za-z0-9_-]+)\.sql$`)

func loadStreams(sources Sources) ([]stream, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("at least one Source is required: %w", ErrInvalidSource)
	}
	seen := make(map[string]bool, len(sources))
	streams := make([]stream, 0, len(sources))
	for i, src := range sources {
		if src.FS == nil {
			return nil, fmt.Errorf("nil FS in Sources[%d] (stream %q): %w", i, src.Stream, ErrInvalidSource)
		}
		if err := validateCleanRel(src.Dir); err != nil {
			return nil, fmt.Errorf("Sources[%d].Dir: %v: %w", i, err, ErrInvalidSource)
		}
		if err := validateCleanRel(src.Stream); err != nil {
			return nil, fmt.Errorf("Sources[%d].Stream: %v: %w", i, err, ErrInvalidSource)
		}
		if seen[src.Stream] {
			return nil, fmt.Errorf("stream %q declared by two sources: %w", src.Stream, ErrDuplicateStream)
		}
		seen[src.Stream] = true

		fsys := src.FS
		if src.Dir != "" {
			sub, err := fs.Sub(fsys, src.Dir)
			if err != nil {
				return nil, fmt.Errorf("Sources[%d].Dir %q: %v: %w", i, src.Dir, err, ErrInvalidSource)
			}
			fsys = sub
		}
		migs, err := loadMigrations(fsys, src.Stream)
		if err != nil {
			return nil, err
		}
		streams = append(streams, stream{name: src.Stream, migs: migs})
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].name < streams[j].name })
	return streams, nil
}

// validateCleanRel accepts "" and clean relative slash paths.
func validateCleanRel(p string) error {
	if p == "" {
		return nil
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("%q has a leading slash", p)
	}
	if strings.HasSuffix(p, "/") {
		return fmt.Errorf("%q has a trailing slash", p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%q has an empty segment", p)
		case ".", "..":
			return fmt.Errorf("%q has a %q segment", p, seg)
		}
	}
	if path.Clean(p) != p {
		return fmt.Errorf("%q is not normalised (path.Clean would change it)", p)
	}
	return nil
}

func displayName(stream string, version int64, name string) string {
	n := fmt.Sprintf("%d_%s", version, name)
	if stream != "" {
		return stream + "/" + n
	}
	return n
}

func loadMigrations(source fs.FS, stream string) ([]Migration, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, fmt.Errorf("read source (stream %q): %w", stream, err)
	}

	var migs []Migration
	seen := map[int64]string{}
	for _, e := range entries {
		// Reject nested files instead of silently ignoring them.
		if e.IsDir() {
			return nil, fmt.Errorf("%q is a directory (stream %q): migration trees are flat: %w", e.Name(), stream, ErrInvalidSource)
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		match := filenameRE.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("%q (want NNNN_name.sql): %w", name, ErrInvalidFilename)
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid version in %q: %w", name, err)
		}
		if prev, ok := seen[version]; ok {
			return nil, fmt.Errorf("version %d in %q and %q: %w", version, prev, name, ErrDuplicateVersion)
		}
		seen[version] = name
		body, err := fs.ReadFile(source, name)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", name, err)
		}
		sum := sha256.Sum256(body)
		migs = append(migs, Migration{
			Version:  version,
			Name:     match[2],
			Body:     string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(migs, func(i, j int) bool { return migs[i].Version < migs[j].Version })
	return migs, nil
}

func loadApplied(ctx context.Context, q Querier) (map[appliedKey]appliedRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT stream, version, name, checksum, applied_at FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query cleanup; rows.Err reports iteration errors

	applied := map[appliedKey]appliedRow{}
	for rows.Next() {
		var k appliedKey
		var r appliedRow
		if err := rows.Scan(&k.stream, &k.version, &r.name, &r.checksum, &r.appliedAt); err != nil {
			return nil, err
		}
		applied[k] = r
	}
	return applied, rows.Err()
}

// sortedKeys keeps error selection deterministic.
func sortedKeys(applied map[appliedKey]appliedRow) []appliedKey {
	keys := make([]appliedKey, 0, len(applied))
	for k := range applied {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].stream != keys[j].stream {
			return keys[i].stream < keys[j].stream
		}
		return keys[i].version < keys[j].version
	})
	return keys
}
