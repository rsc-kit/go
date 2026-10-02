package rsckit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func answer(t *testing.T, h *CallbackHandler, function string) map[string]any {
	t.Helper()

	body, _ := json.Marshal(map[string]any{"function": function, "args": []any{}})
	req := httptest.NewRequest(http.MethodPost, "/__rsc/host-call", bytes.NewReader(body))
	req.Header.Set(SecretHeader, "s")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)

	return out
}

func debugHandler(t *testing.T, on bool) *CallbackHandler {
	t.Helper()

	reg := NewRegistry()
	reg.Handle("Orders.panics", func() string { panic("index out of range") })
	reg.Handle("Orders.fails", func() error {
		return fmt.Errorf("loading orders: %w", errors.New("connection refused"))
	})
	reg.Handle("Orders.refuses", func() error { return Unauthorized() })

	h, _ := NewCallbackHandler(reg, "s")
	h.Debug = on

	return h
}

func TestADebugFailureSaysWhereInGo(t *testing.T) {
	h := debugHandler(t, true)

	panicked := answer(t, h, "Orders.panics")["debug"].(map[string]any)
	trace := fmt.Sprint(panicked["trace"])

	if panicked["type"] != "panic(string)" || !strings.Contains(trace, "debug_test.go") {
		t.Fatalf("panic debug = %v", panicked)
	}

	failed := answer(t, h, "Orders.fails")["debug"].(map[string]any)
	if !strings.Contains(fmt.Sprint(failed["trace"]), "connection refused") {
		t.Fatalf("error debug = %v", failed)
	}
}

func TestNoTraceWithoutDebugOrOnARefusal(t *testing.T) {
	if _, ok := answer(t, debugHandler(t, false), "Orders.panics")["debug"]; ok {
		t.Fatal("a trace without Debug")
	}

	if _, ok := answer(t, debugHandler(t, true), "Orders.refuses")["debug"]; ok {
		t.Fatal("a trace on a refusal")
	}

}
