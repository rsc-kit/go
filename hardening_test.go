package rsckit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// A result whose own MarshalJSON panics. encoding/json re-panics it, and in a
// batch that happened on a goroutine nothing recovered: the process died.
type explodes struct{}

func (explodes) MarshalJSON() ([]byte, error) { panic("boom") }

func quietLogs(t *testing.T) *[]string {
	t.Helper()

	var (
		mu   sync.Mutex
		seen []string
	)

	was := Logger
	Logger = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { Logger = was })

	return &seen
}

func batchLines(t *testing.T, rec *httptest.ResponseRecorder) map[int]batchLine {
	t.Helper()

	lines := map[int]batchLine{}
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))

	for scanner.Scan() {
		var line batchLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("not a JSON line: %s", scanner.Text())
		}

		lines[line.Index] = line
	}

	return lines
}

func TestAResultThatPanicsWhileEncodingIsThatCallsFailureInABatch(t *testing.T) {
	logs := quietLogs(t)
	h := handler(t, func(r *Registry) {
		r.Register("bad", func(context.Context, Args) (any, error) { return explodes{}, nil })
		r.Register("good", func(context.Context, Args) (any, error) { return "fine", nil })
	})

	rec := post(h, `{"calls":[{"function":"bad"},{"function":"good"}]}`, nil)
	lines := batchLines(t, rec)

	if lines[0].Status != http.StatusInternalServerError || !strings.Contains(lines[0].Error, "panicked") {
		t.Fatalf("bad call = %+v, want its own 500", lines[0])
	}

	if lines[1].Status != http.StatusOK || lines[1].Result != "fine" {
		t.Fatalf("sibling = %+v, want answered", lines[1])
	}

	if len(*logs) == 0 {
		t.Fatal("the failure was not logged")
	}
}

func TestAResultThatCannotBeEncodedIsA500WithItsReasonNotAnEmpty200(t *testing.T) {
	quietLogs(t)
	h := handler(t, func(r *Registry) {
		r.Register("nan", func(context.Context, Args) (any, error) { return math.NaN(), nil })
	})

	rec := post(h, `{"function":"nan"}`, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	if reply := decode(t, rec); reply.Error == "" {
		t.Fatal("the reason was lost")
	}
}

func TestABatchLargerThanTheRenderersLimitIsRefused(t *testing.T) {
	var ran atomic.Int32
	h := handler(t, func(r *Registry) {
		r.Register("f", func(context.Context, Args) (any, error) { ran.Add(1); return nil, nil })
	})

	calls := strings.TrimSuffix(strings.Repeat(`{"function":"f"},`, MaxBatch+1), ",")
	rec := post(h, `{"calls":[`+calls+`]}`, nil)

	if rec.Code != http.StatusRequestEntityTooLarge || ran.Load() != 0 {
		t.Fatalf("status = %d, ran = %d; want 413 and nothing run", rec.Code, ran.Load())
	}
}

func TestBatchedCallsEachGetTheirOwnHeaders(t *testing.T) {
	// Run with -race: the calls are concurrent, and each mutates what it got.
	h := handler(t, func(r *Registry) {
		r.Register("f", func(ctx context.Context, _ Args) (any, error) {
			headers := HeadersFrom(ctx)
			headers.Set("Cookie", "mine")

			return headers.Get("Cookie"), nil
		})
	})

	calls := strings.TrimSuffix(strings.Repeat(`{"function":"f"},`, 20), ",")
	rec := post(h, `{"calls":[`+calls+`]}`, map[string]string{"Cookie": "visitor"})

	if len(batchLines(t, rec)) != 20 {
		t.Fatalf("answered %d of 20", len(batchLines(t, rec)))
	}
}

func TestEveryValueOfAForwardedHeaderArrives(t *testing.T) {
	var seen []string
	h := handler(t, func(r *Registry) {
		r.Register("f", func(ctx context.Context, _ Args) (any, error) {
			seen = HeadersFrom(ctx).Values("Cookie")

			return nil, nil
		})
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"function":"f"}`))
	req.Header.Set(SecretHeader, secret)
	req.Header.Add("Cookie", "a=1")
	req.Header.Add("Cookie", "b=2")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(seen) != 2 {
		t.Fatalf("cookies = %v, want both", seen)
	}
}

func TestRefusalsThatCouldReadAsSuccessStillFail(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
	}{
		"a refusal status that is not one": {Refuse(0, "nope"), http.StatusInternalServerError},
		"a 200 refusal":                    {Refuse(200, "nope"), http.StatusInternalServerError},
		"a redirect to nowhere":            {Redirect(""), http.StatusInternalServerError},
		"an empty validation refusal":      {Invalid(nil), http.StatusUnprocessableEntity},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := handler(t, func(r *Registry) {
				r.Register("f", func(context.Context, Args) (any, error) { return nil, c.err })
			})

			rec := post(h, `{"function":"f"}`, nil)
			reply := decode(t, rec)

			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d", rec.Code, c.status)
			}

			if c.status == http.StatusUnprocessableEntity && len(reply.ValidationErrors) == 0 {
				t.Fatal("a 422 without its fields reads as a failed call, not a refusal")
			}

			if c.status == http.StatusInternalServerError && reply.Error == "" {
				t.Fatal("no reason given")
			}
		})
	}
}

func TestAHandlerBuiltWithoutASecretAdmitsNothing(t *testing.T) {
	h := &CallbackHandler{registry: NewRegistry()}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"function":"__rsc.middleware","args":[[]]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAPanicIsLoggedWithWhereItHappened(t *testing.T) {
	logs := quietLogs(t)
	h := handler(t, func(r *Registry) {
		r.Register("Orders.recent", func(context.Context, Args) (any, error) { panic("nil map") })
	})

	post(h, `{"function":"Orders.recent"}`, nil)

	if len(*logs) != 1 || !strings.Contains((*logs)[0], `"Orders.recent"`) || !strings.Contains((*logs)[0], "goroutine") {
		t.Fatalf("logs = %v, want the function named with a stack", *logs)
	}
}

func TestAnUnreachableRendererDoesNotShowTheVisitorWhereItLives(t *testing.T) {
	logs := quietLogs(t)
	r, err := NewRenderer("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Fatalf("body = %q, names the renderer's address", rec.Body.String())
	}

	if len(*logs) != 1 || !strings.Contains((*logs)[0], "127.0.0.1") {
		t.Fatalf("logs = %v, want the reason there instead", *logs)
	}
}
