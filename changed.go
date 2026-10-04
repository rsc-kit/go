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
// version another instance moved. A Changed in this process wakes it at once,
// and so does a WakeOn listener - with one connected, the store is not read
// while a call is held at all.
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

// KeepVersions is how long a name nobody changes is kept: what Prune deletes
// by default, and what MemoryVersions forgets on its own.
const KeepVersions = 30 * 24 * time.Hour

// NextVersion is the version a name moves to: the larger of one past where
// it was and the current time in milliseconds.
//
// A counter would do for "it moved", but a counter repeats once its row is
// gone - deleted to keep the store small, a name starts again from 0 and
// climbs back to a value some tab is still holding, and that tab misses the
// change. A time never comes round again, so a name may be deleted at any
// moment: a tab holding the old value sees a different one and refreshes
// once. It is also when the name last changed, which is all cleanup needs.
// Every store bumps with it; a writer doing + 1 still works, but is not safe
// to prune.
func NextVersion(current int64) int64 {
	if now := time.Now().UnixMilli(); now > current+1 {
		return now
	}

	return current + 1
}

func cutoff(olderThan time.Duration) int64 {
	if olderThan <= 0 {
		olderThan = KeepVersions
	}

	return time.Now().Add(-olderThan).UnixMilli()
}

// MemoryVersions keeps versions in this process: the default, right for one
// instance and for tests. A name not changed in ForgetAfter (KeepVersions by
// default) is forgotten, swept at most once an hour, so a process up for
// months does not hold every name it ever saw.
type MemoryVersions struct {
	ForgetAfter time.Duration

	mu       sync.Mutex
	versions map[string]int64
	swept    time.Time
}

// NewMemoryVersions returns an empty in-process store.
func NewMemoryVersions() *MemoryVersions {
	return &MemoryVersions{versions: make(map[string]int64), swept: time.Now()}
}

// Bump moves each name's version to NextVersion.
func (m *MemoryVersions) Bump(_ context.Context, names []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	forget := m.ForgetAfter
	if forget <= 0 {
		forget = KeepVersions
	}

	if every := min(forget, time.Hour); time.Since(m.swept) >= every {
		m.prune(cutoff(forget))
		m.swept = time.Now()
	}

	for _, name := range names {
		m.versions[name] = NextVersion(m.versions[name])
	}

	return nil
}

// Prune forgets every name not changed in olderThan (KeepVersions when 0).
func (m *MemoryVersions) Prune(_ context.Context, olderThan time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.prune(cutoff(olderThan))

	return nil
}

func (m *MemoryVersions) prune(before int64) {
	for name, version := range m.versions {
		if version < before {
			delete(m.versions, name)
		}
	}
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
	// Notify, on Postgres, is a channel to pg_notify after each Bump, so the
	// other instances - listening through WakeOn - hear at once instead of
	// within ChangedPoll. The notification carries nothing: it only says
	// "ask". Empty sends none.
	Notify string
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

// Bump moves each name's version to NextVersion, creating the row the first
// time. CASE rather than GREATEST, which SQLite spells MAX; the time is this
// process's, passed in, so the database's clock never has to agree.
func (s *SQLVersions) Bump(ctx context.Context, names []string) error {
	update := fmt.Sprintf("UPDATE %s SET version = CASE WHEN version + 1 > %s THEN version + 1 ELSE %s END WHERE name = %s",
		s.table(), s.placeholder(1), s.placeholder(2), s.placeholder(3))
	insert := fmt.Sprintf("INSERT INTO %s (name, version) VALUES (%s, %s)", s.table(), s.placeholder(1), s.placeholder(2))

	for _, name := range names {
		now := time.Now().UnixMilli()

		res, err := s.DB.ExecContext(ctx, update, now, now, name)
		if err != nil {
			return err
		}

		if n, _ := res.RowsAffected(); n > 0 {
			continue
		}

		// New: insert, and if another instance inserted it first, bump that.
		if _, err := s.DB.ExecContext(ctx, insert, name, now); err != nil {
			if _, retry := s.DB.ExecContext(ctx, update, now, now, name); retry != nil {
				return err
			}
		}
	}

	if s.Notify != "" {
		_, err := s.DB.ExecContext(ctx, fmt.Sprintf("SELECT pg_notify(%s, '')", s.placeholder(1)), s.Notify)

		return err
	}

	return nil
}

// Prune deletes every name not changed in olderThan (KeepVersions when 0).
// Always safe: a version never comes round again, so a tab still holding a
// pruned name sees it differ and refreshes once. Run it from a scheduled job.
func (s *SQLVersions) Prune(ctx context.Context, olderThan time.Duration) error {
	_, err := s.DB.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE version < %s", s.table(), s.placeholder(1)), cutoff(olderThan))

	return err
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

	r.wake()

	return nil
}

// wake answers every ChangedFunction call this process is holding: each reads
// the store again and returns what moved.
func (r *Registry) wake() {
	r.versionsMu.Lock()
	if r.moved != nil {
		close(r.moved)
	}
	r.moved = make(chan struct{})
	r.versionsMu.Unlock()
}

// WakeOn hears changes made by other instances the moment they are made,
// rather than within ChangedPoll - and stops the store being read every
// second while nothing changes.
//
// listen connects to whatever announces a change - Postgres LISTEN on the
// channel SQLVersions.Notify names, Redis pub/sub, a broadcast server - calls
// connected once it is listening, calls wake for each announcement, and
// returns when the connection ends. It is run until ctx is done, again with
// backoff after it returns an error; while it is down, held calls go back to
// reading the store every ChangedPoll, so nothing is missed meanwhile.
//
// With pgx, which the adapter does not depend on:
//
//	reg.WakeOn(ctx, func(ctx context.Context, connected, wake func()) error {
//		conn, err := pool.Acquire(ctx)
//		if err != nil {
//			return err
//		}
//		defer conn.Release()
//		if _, err := conn.Exec(ctx, "LISTEN rsc_versions"); err != nil {
//			return err
//		}
//		connected()
//		for {
//			if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
//				return err
//			}
//			wake()
//		}
//	})
func (r *Registry) WakeOn(ctx context.Context, listen func(ctx context.Context, connected, wake func()) error) {
	go func() {
		failures := 0

		for ctx.Err() == nil {
			err := listen(ctx, func() {
				r.listening.Store(true)
				// Connected again after a drop: whatever was announced
				// meanwhile is gone, so ask now.
				r.wake()
			}, r.wake)

			// It had connected: the backoff starts over. And the poller,
			// which was waiting only for this listener, goes back to the poll.
			if r.listening.Swap(false) {
				failures = 0
				r.wake()
			}

			if ctx.Err() != nil {
				return
			}

			if failures == 0 {
				Logger("rsc-kit: the WakeOn listener stopped (%v); checking the store every ChangedPoll until it is back", err)
			}

			pause := time.Second << min(failures, 5)
			failures++

			select {
			case <-ctx.Done():
				return
			case <-time.After(pause):
			}
		}
	}()
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

	// Taken before the read: a change between the read and joining the
	// poller would otherwise go unseen until the poller's next round.
	before := r.movedCh()

	versions, err := store.Versions(ctx, names)
	if err != nil {
		return nil, err
	}

	differ := differing(query.Since, versions)
	if len(differ) > 0 || wait <= 0 {
		return map[string]any{"versions": differ}, nil
	}

	// Held: the shared poller answers it, reading the store once for every
	// call held here rather than once each.
	w := &waiter{since: query.Since, answer: make(chan map[string]int64, 1)}
	r.hold(w)
	defer r.release(w)

	select {
	case <-before:
		r.wake()
	default:
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case moved := <-w.answer:
		return map[string]any{"versions": moved}, nil
	case <-timer.C:
		// Nothing moved that the last read saw. One that moved since is in
		// the renderer's next ask, which starts from the same versions.
		return map[string]any{"versions": map[string]int64{}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// differing is every name in since whose version is not the one held.
func differing(since, versions map[string]int64) map[string]int64 {
	differ := make(map[string]int64)
	for name, held := range since {
		if versions[name] != held {
			differ[name] = versions[name]
		}
	}

	return differ
}

// A waiter is one held ChangedFunction call: what it holds, and where its answer goes.
type waiter struct {
	since  map[string]int64
	answer chan map[string]int64
}

// hold registers a held call, starting the poller if none is running.
func (r *Registry) hold(w *waiter) {
	r.versionsMu.Lock()
	defer r.versionsMu.Unlock()

	if r.waiters == nil {
		r.waiters = make(map[*waiter]struct{})
	}
	r.waiters[w] = struct{}{}

	if !r.polling {
		r.polling = true
		// Read once, here: the poller outlives the call that started it.
		go r.poll(ChangedPoll)
	}
}

func (r *Registry) release(w *waiter) {
	r.versionsMu.Lock()
	defer r.versionsMu.Unlock()

	delete(r.waiters, w)
}

// poll answers every held call from one read of the store - the names all of
// them hold, together - each ChangedPoll, or at once when woken by a Changed
// here or a WakeOn listener. With a listener connected it reads only when
// woken. However many renderers, isolates or tabs are waiting, the store is
// read once per round. It stops when nothing is held.
func (r *Registry) poll(every time.Duration) {
	ctx := context.Background()

	for {
		// Taken before the read, so a wake between the read and the wait
		// closes this one rather than one made afterwards.
		moved := r.movedCh()

		r.versionsMu.Lock()
		if len(r.waiters) == 0 {
			r.polling = false
			r.versionsMu.Unlock()
			return
		}
		held := make([]*waiter, 0, len(r.waiters))
		union := make(map[string]struct{})
		for w := range r.waiters {
			held = append(held, w)
			for name := range w.since {
				union[name] = struct{}{}
			}
		}
		r.versionsMu.Unlock()

		names := make([]string, 0, len(union))
		for name := range union {
			names = append(names, name)
		}
		sort.Strings(names)

		// A read that fails answers nobody: each held call runs out its wait,
		// and the renderer's next ask reports the error.
		if versions, err := r.versionStore().Versions(ctx, names); err == nil {
			for _, w := range held {
				if differ := differing(w.since, versions); len(differ) > 0 {
					select {
					case w.answer <- differ:
					default:
					}
					r.release(w)
				}
			}
		}

		var tick <-chan time.Time
		var timer *time.Timer
		if !r.listening.Load() {
			timer = time.NewTimer(every)
			tick = timer.C
		}

		select {
		case <-moved:
		case <-tick:
		}

		if timer != nil {
			timer.Stop()
		}
	}
}
