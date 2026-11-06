package rsckit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Purging: Changed tells the CDN keeping pages, once the version has moved.

type purges struct {
	mu   sync.Mutex
	sent []Purge
	got  chan struct{}
}

func recordPurges() (*purges, Purger) {
	p := &purges{got: make(chan struct{}, 100)}

	return p, func(_ context.Context, purge Purge) error {
		p.mu.Lock()
		p.sent = append(p.sent, purge)
		p.mu.Unlock()
		p.got <- struct{}{}

		return nil
	}
}

func (p *purges) wait(t *testing.T) []Purge {
	t.Helper()

	select {
	case <-p.got:
	case <-time.After(2 * time.Second):
		t.Fatal("no purge was sent")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]Purge(nil), p.sent...)
}

func TestAHostCallPurgesEveryNameItChangedOnceItHasAnswered(t *testing.T) {
	reg := NewRegistry()
	recorded, purger := recordPurges()
	reg.SetPurger(purger)

	reg.Handle("Posts.save", func(ctx context.Context) (string, error) {
		if err := reg.Changed(ctx, "posts", "post:1"); err != nil {
			return "", err
		}

		recorded.mu.Lock()
		early := len(recorded.sent)
		recorded.mu.Unlock()
		if early != 0 {
			t.Error("purged before the call answered")
		}

		return "ok", reg.Changed(ctx, "posts")
	})

	h, _ := NewCallbackHandler(reg, "s")
	rec := post(h, `{"function":"Posts.save","args":[]}`, map[string]string{SecretHeader: "s"})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	sent := recorded.wait(t)
	if len(sent) != 1 || strings.Join(sent[0].Tags, ",") != "posts,post:1" {
		t.Fatalf("sent %+v", sent)
	}
}

func TestChangedOutsideAHostCallPurgesInTheBackground(t *testing.T) {
	reg := NewRegistry()
	recorded, purger := recordPurges()
	reg.SetPurger(purger)

	if err := reg.Changed(context.Background(), "posts"); err != nil {
		t.Fatal(err)
	}

	if sent := recorded.wait(t); strings.Join(sent[0].Tags, ",") != "posts" {
		t.Fatalf("sent %+v", sent)
	}
}

func TestHomeportIsTheDefaultAndSetPurgerNilTurnsItOff(t *testing.T) {
	t.Setenv("HOMEPORT_PURGE_URL", "http://purge.internal/")

	reg := NewRegistry()
	if reg.currentPurger() == nil {
		t.Fatal("HOMEPORT_PURGE_URL set, and no purge")
	}

	reg.SetPurger(nil)
	if reg.currentPurger() != nil {
		t.Fatal("SetPurger(nil) left a purge")
	}
}

func TestAFailedPurgeIsLoggedAndNeverFailsTheChange(t *testing.T) {
	logged := make(chan string, 1)
	old := Logger
	Logger = func(format string, args ...any) { logged <- format }
	defer func() { Logger = old }()

	reg := NewRegistry()
	reg.SetPurger(func(context.Context, Purge) error { return errors.New("CDN down") })

	if err := reg.Changed(context.Background(), "posts"); err != nil {
		t.Fatalf("the change failed: %v", err)
	}

	select {
	case <-logged:
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was logged")
	}
}

func TestHomeportPurgerSendsJSONInCallsOfAtMost100(t *testing.T) {
	var mu sync.Mutex
	var bodies []Purge
	status := []int{http.StatusTooManyRequests}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s with %q", r.Method, r.Header.Get("Content-Type"))
		}

		var body Purge
		_ = json.NewDecoder(r.Body).Decode(&body)

		mu.Lock()
		defer mu.Unlock()

		bodies = append(bodies, body)

		if len(status) > 0 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(status[0])
			status = status[1:]

			return
		}

		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	tags := make([]string, 250)
	for i := range tags {
		tags[i] = "t" + string(rune('a'+i%26))
	}

	if err := HomeportPurger(server.URL, nil)(context.Background(), Purge{Tags: tags}); err != nil {
		t.Fatal(err)
	}

	// The 429 tried again, then three calls: 100, 100, 50.
	if len(bodies) != 4 || len(bodies[0].Tags) != 100 || len(bodies[3].Tags) != 50 {
		t.Fatalf("sent %d calls", len(bodies))
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer refusing.Close()

	if err := HomeportPurger(refusing.URL, nil)(context.Background(), Purge{Tags: []string{"a"}}); err == nil {
		t.Fatal("a 400 was not an error")
	}
}
