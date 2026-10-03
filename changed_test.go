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
	if moved["orders"] != 2 || len(moved) != 1 {
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

	if moved["orders"] != 1 {
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
	if moved["orders"] != 1 {
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
