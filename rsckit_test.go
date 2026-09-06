package rsckit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const secret = "s3cret"

func handler(t *testing.T, register func(*Registry)) *CallbackHandler {
	t.Helper()

	reg := NewRegistry()
	register(reg)

	h, err := NewCallbackHandler(reg, secret)
	if err != nil {
		t.Fatalf("NewCallbackHandler: %v", err)
	}

	return h
}

func post(h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/__rsc/host-call", strings.NewReader(body))
	req.Header.Set(SecretHeader, secret)

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) callReply {
	t.Helper()

	var reply callReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("reply is not JSON (%d): %s", rec.Code, rec.Body.String())
	}

	return reply
}

func TestCallsAFunctionAndReturnsItsResult(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("Orders.recent", func(_ context.Context, args Args) (any, error) {
			var limit int
			if err := args.Bind(&limit); err != nil {
				return nil, err
			}

			return map[string]any{"limit": limit}, nil
		})
	})

	rec := post(h, `{"function":"Orders.recent","args":[5]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	result := decode(t, rec).Result.(map[string]any)
	if result["limit"].(float64) != 5 {
		t.Fatalf("limit = %v, want 5", result["limit"])
	}
}

// The reason the endpoint exists: a host call runs as the person browsing.
func TestForwardsTheVisitorsSessionToTheFunction(t *testing.T) {
	var seen string

	h := handler(t, func(r *Registry) {
		r.Register("Me.name", func(ctx context.Context, _ Args) (any, error) {
			seen = HeadersFrom(ctx).Get("Cookie")

			return seen, nil
		})
	})

	post(h, `{"function":"Me.name","args":[]}`, map[string]string{"Cookie": "session=abc"})

	if seen != "session=abc" {
		t.Fatalf("cookie = %q, want session=abc", seen)
	}
}

func TestRefusesACallWithoutTheSecret(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("X.y", func(context.Context, Args) (any, error) {
			t.Fatal("must not run")

			return nil, nil
		})
	})

	req := httptest.NewRequest(http.MethodPost, "/__rsc/host-call", strings.NewReader(`{"function":"X.y","args":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRefusesACallWithTheWrongSecret(t *testing.T) {
	h := handler(t, func(r *Registry) {})

	req := httptest.NewRequest(http.MethodPost, "/__rsc/host-call", strings.NewReader(`{"function":"X.y","args":[]}`))
	req.Header.Set(SecretHeader, "nope")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestACallbackHandlerNeedsASecret(t *testing.T) {
	if _, err := NewCallbackHandler(NewRegistry(), ""); err != ErrNoSecret {
		t.Fatalf("err = %v, want ErrNoSecret", err)
	}
}

// The JS side cannot say which function is missing — it does not know what
// this host registered — so this side has to.
func TestAnUnknownFunctionNamesItselfAndWhatExists(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("Orders.recent", func(context.Context, Args) (any, error) { return nil, nil })
	})

	rec := post(h, `{"function":"Orders.recnt","args":[]}`, nil)
	reply := decode(t, rec)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	if !strings.Contains(reply.Error, "Orders.recnt") || !strings.Contains(reply.Error, "Orders.recent") {
		t.Fatalf("error should name the typo and the real name, got %q", reply.Error)
	}
}

func TestAnErrorFromAFunctionBecomesTheReportedError(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("Orders.recent", func(context.Context, Args) (any, error) {
			return nil, io.ErrUnexpectedEOF
		})
	})

	reply := decode(t, post(h, `{"function":"Orders.recent","args":[]}`, nil))
	if !strings.Contains(reply.Error, "unexpected EOF") {
		t.Fatalf("error = %q", reply.Error)
	}
}

// One component failing to fetch must not take down every render in flight.
func TestAPanickingFunctionBecomesAnErrorRatherThanKillingTheServer(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("Boom", func(context.Context, Args) (any, error) {
			panic("nil map")
		})
	})

	rec := post(h, `{"function":"Boom","args":[]}`, nil)
	reply := decode(t, rec)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	if !strings.Contains(reply.Error, "panicked") || !strings.Contains(reply.Error, "nil map") {
		t.Fatalf("error = %q", reply.Error)
	}
}

func TestMalformedBodyIsRejectedRatherThanDecoded(t *testing.T) {
	h := handler(t, func(r *Registry) {})

	rec := post(h, `{"function":`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestReportsWhatAFunctionInvalidated(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("Orders.create", func(ctx context.Context, _ Args) (any, error) {
			Revalidate(ctx, "orders", "page")

			return "created", nil
		})
	})

	reply := decode(t, post(h, `{"function":"Orders.create","args":[]}`, nil))
	if strings.Join(reply.Revalidate, ",") != "orders,page" {
		t.Fatalf("revalidate = %v", reply.Revalidate)
	}
}

func TestBindReportsTooFewArgumentsRatherThanZeroValues(t *testing.T) {
	var got error

	h := handler(t, func(r *Registry) {
		r.Register("Needs.two", func(_ context.Context, args Args) (any, error) {
			var a, b int
			got = args.Bind(&a, &b)

			return nil, got
		})
	})

	post(h, `{"function":"Needs.two","args":[1]}`, nil)

	if got == nil || !strings.Contains(got.Error(), "wants 2") {
		t.Fatalf("err = %v, want a count mismatch", got)
	}
}

func TestRegisteringTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()

	reg := NewRegistry()
	reg.Register("X", func(context.Context, Args) (any, error) { return nil, nil })
	reg.Register("X", func(context.Context, Args) (any, error) { return nil, nil })
}

// The shell has to reach the browser before the render finishes — that is the
// whole reason a page paints its skeletons immediately.
//
// This does NOT discriminate on FlushInterval: Go forces immediate flushing
// for any response with no Content-Length, which is every streamed render, so
// it passes with the setting removed. It is here to catch the thing that would
// actually break this — a wrapper, middleware or ResponseWriter added in front
// that buffers the body.
func TestTheProxyStreamsRatherThanBuffering(t *testing.T) {
	release := make(chan struct{})

	renderer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!DOCTYPE html><html><body>shell")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "rest</body></html>")
	}))
	defer renderer.Close()

	r, err := NewRenderer(renderer.URL)
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	front := httptest.NewServer(r)
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 32)
	done := make(chan int, 1)

	go func() {
		n, _ := resp.Body.Read(buf)
		done <- n
	}()

	select {
	case n := <-done:
		if !strings.Contains(string(buf[:n]), "shell") {
			t.Fatalf("first read = %q, want the shell", string(buf[:n]))
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the shell did not arrive before the rest — the proxy is buffering")
	}

	close(release)
}

func TestTheProxyReportsAnUnreachableRendererAsSuch(t *testing.T) {
	r, err := NewRenderer("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("NewRenderer: %v", err)
	}

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "rsc renderer unreachable") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestTheHandlerRoutesCallbacksAndPagesApart(t *testing.T) {
	renderer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "page")
	}))
	defer renderer.Close()

	r, _ := NewRenderer(renderer.URL)
	cb := handler(t, func(reg *Registry) {
		reg.Register("X.y", func(context.Context, Args) (any, error) { return "called", nil })
	})

	h := NewHandler(r, cb, "/__rsc/host-call")
	front := httptest.NewServer(h)
	defer front.Close()

	page, err := http.Get(front.URL + "/docs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer page.Body.Close()

	body, _ := io.ReadAll(page.Body)
	if string(body) != "page" {
		t.Fatalf("page body = %q", body)
	}

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/__rsc/host-call", strings.NewReader(`{"function":"X.y","args":[]}`))
	req.Header.Set(SecretHeader, secret)

	call, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer call.Body.Close()

	callBody, _ := io.ReadAll(call.Body)
	if !strings.Contains(string(callBody), "called") {
		t.Fatalf("callback body = %q", callBody)
	}
}

// A refusal is an answer, not a failure. The renderer has to be able to tell
// them apart without parsing a message: one becomes field errors under the
// inputs, the other becomes a 500 nobody should be able to cause.
func TestARefusalIsA422WithItsFieldsIntact(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("Orders.create", func(context.Context, Args) (any, error) {
			return nil, Invalid(map[string][]string{
				"name":     {"The name field is required."},
				"quantity": {"The quantity must be a number.", "The quantity must be at least 1."},
			})
		})
	})

	rec := post(h, `{"function":"Orders.create","args":[]}`, nil)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}

	reply := decode(t, rec)

	if reply.Error != "" {
		t.Fatalf("a refusal must not also report an error, got %q", reply.Error)
	}

	if got := reply.ValidationErrors["name"]; len(got) != 1 || got[0] != "The name field is required." {
		t.Fatalf("name = %v", got)
	}

	if got := reply.ValidationErrors["quantity"]; len(got) != 2 {
		t.Fatalf("several messages for one field must survive, got %v", got)
	}
}

func TestInvalidFieldIsTheSingleFieldCase(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("X.y", func(context.Context, Args) (any, error) {
			return nil, InvalidField("email", "Already taken.")
		})
	})

	reply := decode(t, post(h, `{"function":"X.y","args":[]}`, nil))

	if got := reply.ValidationErrors["email"]; len(got) != 1 || got[0] != "Already taken." {
		t.Fatalf("email = %v", got)
	}
}

// A message about the form rather than any one field goes under the empty key,
// which is where the form component looks for it.
func TestAFormLevelMessageUsesTheEmptyKey(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("X.y", func(context.Context, Args) (any, error) {
			return nil, InvalidField("", "This form has already been submitted.")
		})
	})

	reply := decode(t, post(h, `{"function":"X.y","args":[]}`, nil))

	if got := reply.ValidationErrors[""]; len(got) != 1 {
		t.Fatalf("form-level message = %v", got)
	}
}

func TestAnOrdinaryErrorIsStillAFailureNotARefusal(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("X.y", func(context.Context, Args) (any, error) {
			return nil, io.ErrUnexpectedEOF
		})
	})

	rec := post(h, `{"function":"X.y","args":[]}`, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	if decode(t, rec).ValidationErrors != nil {
		t.Fatal("a failure must not be reported as field errors")
	}
}
