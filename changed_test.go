package rsckit

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Tags: a version per name, moved by Changed, asked for by the renderer as
// "the ones that differ from what I hold" - and held until one does.

func askChanged(t *testing.T, h *CallbackHandler, since map[string]int64, wait int64) map[string]int64 {
	t.Helper()

	body, _ := json.Marshal(map[string]any{
		"function": ChangedFunction,
		"args":     []any{map[string]any{"since": since, "wait": wait}},
	})
	rec := post(h, string(body), nil)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var reply struct {
		Result struct {
			Versions map[string]int64 `json:"versions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("reply is not JSON: %s", rec.Body.String())
	}
	if reply.Result.Versions == nil {
		t.Fatalf("versions must be an object, even empty: %s", rec.Body.String())
	}

	return reply.Result.Versions
}

func TestANameNobodyChangedIsAtZero(t *testing.T) {
	h := handler(t, func(*Registry) {})

	all := askChanged(t, h, map[string]int64{"orders": -1}, 0)
	if all["orders"] != 0 {
		t.Fatalf("from -1: %v", all)
	}

	none := askChanged(t, h, map[string]int64{"orders": 0}, 0)
	if len(none) != 0 {
		t.Fatalf("from 0: %v", none)
	}
}

func TestChangedMovesTheVersionAndOnlyWhatDiffersIsAnswered(t *testing.T) {
	var reg *Registry
	h := handler(t, func(r *Registry) { reg = r })

	if err := reg.Changed(context.Background(), "orders", "orders"); err != nil {
		t.Fatal(err)
	}

	moved := askChanged(t, h, map[string]int64{"orders": 0, "stock": 0}, 0)
	if moved["orders"] < time.Now().Add(-time.Minute).UnixMilli() || len(moved) != 1 {
		t.Fatalf("got %v", moved)
	}
}

func TestAHeldAskIsAnsweredTheMomentANameMovesHere(t *testing.T) {
	var reg *Registry
	h := handler(t, func(r *Registry) { reg = r })

	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = reg.Changed(context.Background(), "orders")
	}()

	started := time.Now()
	moved := askChanged(t, h, map[string]int64{"orders": 0}, 5_000)

	if moved["orders"] <= 0 {
		t.Fatalf("got %v", moved)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("held %v after the change", time.Since(started))
	}
}

func TestAHeldAskSeesAnotherInstanceMoveTheStore(t *testing.T) {
	// Another instance bumps the shared store directly: no Changed here to
	// wake the wait, so the poll is what notices.
	store := NewMemoryVersions()
	h := handler(t, func(r *Registry) { r.Versions(store) })

	old := ChangedPoll
	ChangedPoll = 20 * time.Millisecond
	defer func() { ChangedPoll = old }()

	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = store.Bump(context.Background(), []string{"orders"})
	}()

	moved := askChanged(t, h, map[string]int64{"orders": 0}, 2_000)
	if moved["orders"] <= 0 {
		t.Fatalf("got %v", moved)
	}
}

func TestAHeldAskIsBoundedByWait(t *testing.T) {
	h := handler(t, func(*Registry) {})

	started := time.Now()
	none := askChanged(t, h, map[string]int64{"orders": 0}, 150)

	if len(none) != 0 {
		t.Fatalf("got %v", none)
	}
	if took := time.Since(started); took < 100*time.Millisecond || took > time.Second {
		t.Fatalf("took %v", took)
	}
}

func TestTheReservedChangedNameCannotBeRegistered(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()

	NewRegistry().Register(ChangedFunction, func(context.Context, Args) (any, error) { return nil, nil })
}

func TestChangedIsNotInTheManifest(t *testing.T) {
	reg := NewRegistry()
	for _, name := range reg.Manifest().Functions {
		if name == ChangedFunction || name == MiddlewareFunction {
			t.Fatalf("%q is the engine's, not the app's", name)
		}
	}
}

func TestNextVersionNeverRepeats(t *testing.T) {
	before := time.Now().UnixMilli()

	first := NextVersion(0)
	if first < before {
		t.Fatalf("a first version is now: got %d, before %d", first, before)
	}

	if second := NextVersion(first); second <= first {
		t.Fatalf("a bump moves past where it was: %d then %d", first, second)
	}

	// A counter written by an older writer moves to the time.
	if v := NextVersion(7); v < before {
		t.Fatalf("a counter moves to now: got %d", v)
	}

	// A version ahead of the clock still moves.
	ahead := time.Now().Add(time.Hour).UnixMilli()
	if v := NextVersion(ahead); v != ahead+1 {
		t.Fatalf("ahead of the clock: got %d, want %d", v, ahead+1)
	}
}

func TestMemoryVersionsPruneOldNamesSafely(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVersions()

	_ = store.Bump(ctx, []string{"old", "fresh"})
	held := time.Now().Add(-365 * 24 * time.Hour).UnixMilli()

	store.mu.Lock()
	store.versions["old"] = held
	store.mu.Unlock()

	if err := store.Prune(ctx, 0); err != nil {
		t.Fatal(err)
	}

	versions, _ := store.Versions(ctx, []string{"old", "fresh"})
	if versions["old"] != 0 || versions["fresh"] == 0 {
		t.Fatalf("got %v", versions)
	}

	// Changed again, it comes back past what a tab still holds, never at it.
	_ = store.Bump(ctx, []string{"old"})
	if versions, _ = store.Versions(ctx, []string{"old"}); versions["old"] <= held {
		t.Fatalf("came back at %d, a tab holds %d", versions["old"], held)
	}
}

func TestMemoryVersionsForgetOnTheirOwn(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryVersions()
	store.ForgetAfter = 20 * time.Millisecond

	_ = store.Bump(ctx, []string{"old"})
	time.Sleep(40 * time.Millisecond)
	_ = store.Bump(ctx, []string{"other"}) // a bump sweeps

	if versions, _ := store.Versions(ctx, []string{"old"}); versions["old"] != 0 {
		t.Fatalf("still remembered: %v", versions)
	}
}
