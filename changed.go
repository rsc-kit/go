package rsckit

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tags: a page or a section names the data it shows, and this side says when
// that changed - from a webhook, a job, another visitor's action, anywhere a
// context is at hand. Every open tab showing it refreshes.
//
//	reg.Changed(ctx, "team:"+teamID+":repos")   // in the GitHub webhook handler
//
// A name has a version, a number that moves when Changed names it. Nothing
// else travels: not what changed, not the data. The renderer reads versions
// at render and asks ChangedFunction for the ones that moved since; this side
// holds that ask until one does or the wait runs out, woken at once by a
// Changed in this process and checking the store for the other instances.
//
// Versions live in a VersionStore. The default keeps them in memory, which is
// right for one instance; SQLVersions keeps them in a table every instance
// shares, and anything with a Bump and a Versions - Redis, a cache - fits.

// ChangedFunction is the reserved name the renderer asks name versions on. It is
// answered by the registry itself, never registered by an app.
const ChangedFunction = "__rsc.changed"

// ChangedPoll is how often a held ChangedFunction call checks the store for a
// version another instance moved. A Changed in this process wakes it at once.
var ChangedPoll = time.Second

// MaxChangedWait bounds how long one ChangedFunction call is held, whatever it asked.
const MaxChangedWait = 30 * time.Second

// A VersionStore keeps a version per name.
//
// Shared by every instance of the app when there is more than one: a webhook
// lands on whichever instance the balancer picked, and a tab's stream is held
// by whichever renderer it reached.
type VersionStore interface {
	// Bump moves each name's version.
	Bump(ctx context.Context, names []string) error
	// Versions answers the current version of each name. One never bumped is 0.
	Versions(ctx context.Context, names []string) (map[string]int64, error)
}

// MemoryVersions keeps versions in this process: the default, right for one
// instance and for tests.
type MemoryVersions struct {
	mu       sync.Mutex
	versions map[string]int64
}

// NewMemoryVersions returns an empty in-process store.
func NewMemoryVersions() *MemoryVersions {
	return &MemoryVersions{versions: make(map[string]int64)}
}

// Bump moves each name's version.
func (m *MemoryVersions) Bump(_ context.Context, names []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, name := range names {
		m.versions[name]++
	}

	return nil
}

// Versions answers the current version of each name.
func (m *MemoryVersions) Versions(_ context.Context, names []string) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]int64, len(names))
	for _, name := range names {
		out[name] = m.versions[name]
	}

	return out, nil
}

// SQLVersions keeps versions in a table every instance shares:
//
//	CREATE TABLE rsc_versions (name TEXT PRIMARY KEY, version BIGINT NOT NULL)
//
// Written with an UPDATE and, when the row is new, an INSERT - two statements
// any SQL dialect has, rather than one upsert each spells differently.
// Placeholder writes the dialect's parameter: nil is "?", Dollar is "$1".
type SQLVersions struct {
	DB    *sql.DB
	Table string
	// Placeholder writes the n-th parameter (1-based). nil means "?".
	Placeholder func(n int) string
}

// Dollar is the placeholder Postgres uses: $1, $2, ...
func Dollar(n int) string { return fmt.Sprintf("$%d", n) }

func (s *SQLVersions) placeholder(n int) string {
	if s.Placeholder != nil {
		return s.Placeholder(n)
	}

	return "?"
}

func (s *SQLVersions) table() string {
	if s.Table != "" {
		return s.Table
	}

	return "rsc_versions"
}

// Bump moves each name's version, creating the row the first time.
func (s *SQLVersions) Bump(ctx context.Context, names []string) error {
	update := fmt.Sprintf("UPDATE %s SET version = version + 1 WHERE name = %s", s.table(), s.placeholder(1))
	insert := fmt.Sprintf("INSERT INTO %s (name, version) VALUES (%s, 1)", s.table(), s.placeholder(1))

	for _, name := range names {
		res, err := s.DB.ExecContext(ctx, update, name)
		if err != nil {
			return err
		}

		if n, _ := res.RowsAffected(); n > 0 {
			continue
		}

		// New: insert, and if another instance inserted it first, bump that.
		if _, err := s.DB.ExecContext(ctx, insert, name); err != nil {
			if _, retry := s.DB.ExecContext(ctx, update, name); retry != nil {
				return err
			}
		}
	}

	return nil
}

// Versions answers the current version of each name; a name with no row is 0.
func (s *SQLVersions) Versions(ctx context.Context, names []string) (map[string]int64, error) {
	out := make(map[string]int64, len(names))
	if len(names) == 0 {
		return out, nil
	}

	marks := make([]string, len(names))
	args := make([]any, len(names))
	for i, name := range names {
		marks[i] = s.placeholder(i + 1)
		args[i] = name
		out[name] = 0
	}

	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf("SELECT name, version FROM %s WHERE name IN (%s)", s.table(), strings.Join(marks, ", ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var version int64
		if err := rows.Scan(&name, &version); err != nil {
			return nil, err
		}
		out[name] = version
	}

	return out, rows.Err()
}

// Tags sets where this registry keeps name versions. Unset, they are in memory.
func (r *Registry) Versions(store VersionStore) {
	r.versionsMu.Lock()
	defer r.versionsMu.Unlock()

	r.versions = store
}

func (r *Registry) versionStore() VersionStore {
	r.versionsMu.Lock()
	defer r.versionsMu.Unlock()

	if r.versions == nil {
		r.versions = NewMemoryVersions()
	}

	return r.versions
}

// Changed says these names changed, so every open tab showing them refreshes.
//
// From anywhere with a context: a webhook handler, a job, an action. The
// version moves in the store, and a ChangedFunction call this process is holding
// is answered at once.
func (r *Registry) Changed(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}

	if err := r.versionStore().Bump(ctx, names); err != nil {
		return err
	}

	r.versionsMu.Lock()
	if r.moved != nil {
		close(r.moved)
	}
	r.moved = make(chan struct{})
	r.versionsMu.Unlock()

	return nil
}

// movedCh is closed, and replaced, by the next Changed in this process.
func (r *Registry) movedCh() <-chan struct{} {
	r.versionsMu.Lock()
	defer r.versionsMu.Unlock()

	if r.moved == nil {
		r.moved = make(chan struct{})
	}

	return r.moved
}

// runChanged answers ChangedFunction: of the versions the renderer holds, the ones
// that differ now - held up to wait for one to, woken by Changed here and
// checking the store every ChangedPoll for the other instances.
func (r *Registry) runChanged(ctx context.Context, args Args) (any, error) {
	var query struct {
		Since map[string]int64 `json:"since"`
		Wait  int64            `json:"wait"`
	}
	if err := args.Bind(&query); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(query.Since))
	for name := range query.Since {
		names = append(names, name)
	}
	sort.Strings(names)

	wait := time.Duration(query.Wait) * time.Millisecond
	if wait > MaxChangedWait {
		wait = MaxChangedWait
	}
	deadline := time.Now().Add(wait)
	store := r.versionStore()

	for {
		// Taken before the read, so a Changed between the read and the wait
		// closes this one rather than one made afterwards.
		moved := r.movedCh()

		versions, err := store.Versions(ctx, names)
		if err != nil {
			return nil, err
		}

		differ := make(map[string]int64)
		for name, held := range query.Since {
			if versions[name] != held {
				differ[name] = versions[name]
			}
		}

		remaining := time.Until(deadline)
		if len(differ) > 0 || remaining <= 0 {
			return map[string]any{"versions": differ}, nil
		}

		pause := ChangedPoll
		if remaining < pause {
			pause = remaining
		}

		timer := time.NewTimer(pause)
		select {
		case <-moved:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
		timer.Stop()
	}
}
