package rsckit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type tOrder struct {
	ID      int       `json:"id"`
	Total   float64   `json:"total"`
	Note    string    `json:"note,omitempty"`
	Placed  time.Time `json:"placed"`
	Lines   []tLine   `json:"lines"`
	Parent  *tOrder   `json:"parent"`
	Secret  string    `json:"-"`
	private string    //nolint:unused
}

type tBuild struct {
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

func TestAPointerToTimeIsAStringOrNull(t *testing.T) {
	// *time.Time has time.Time's MarshalJSON, and was read as a type that
	// writes its own shape: unknown, on every optional timestamp.
	reg := NewRegistry()
	reg.Handle("Builds.find", func(id int) (tBuild, error) { return tBuild{}, nil })

	raw, _ := json.Marshal(reg.Manifest().Defs["tBuild"]["properties"])

	var props map[string]any
	_ = json.Unmarshal(raw, &props)

	want := map[string]any{"anyOf": []any{
		map[string]any{"type": "string", "format": "date-time"},
		map[string]any{"type": "null"},
	}}

	for _, name := range []string{"startedAt", "finishedAt"} {
		if !reflect.DeepEqual(props[name], want) {
			t.Fatalf("%s = %v", name, props[name])
		}
	}
}

type tPage struct {
	Rows  []tLine           `json:"rows"`
	Tags  map[string]string `json:"tags"`
	Next  *tPage            `json:"next"`
	Lines [][]tLine         `json:"lines"`
}

func TestANilSliceOrMapIsSentEmptyNotNull(t *testing.T) {
	// "No rows" in Go is usually a nil slice, which encoding/json writes as
	// null - and the TypeScript said Order[], so the page's .map threw.
	reg := NewRegistry()
	reg.Handle("Orders.none", func() ([]tOrder, error) {
		var none []tOrder
		return none, nil
	})
	reg.Handle("Pages.first", func() (tPage, error) {
		return tPage{Lines: [][]tLine{nil}, Next: &tPage{}}, nil
	})

	got, _ := call(t, reg, "Orders.none", Args{})
	if raw, _ := json.Marshal(got); string(raw) != "[]" {
		t.Fatalf("Orders.none = %s", raw)
	}

	got, _ = call(t, reg, "Pages.first", Args{})
	raw, _ := json.Marshal(got)
	want := `{"rows":[],"tags":{},"next":{"rows":[],"tags":{},"next":null,"lines":[]},"lines":[[]]}`

	if string(raw) != want {
		t.Fatalf("Pages.first = %s", raw)
	}
}

type tLine struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

type tFind struct {
	IncludeLines bool `json:"includeLines"`
}

func args(t *testing.T, values ...any) Args {
	t.Helper()

	out := Args{}
	for _, v := range values {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, raw)
	}

	return out
}

func call(t *testing.T, reg *Registry, name string, a Args) (any, error) {
	t.Helper()

	fn, ok := reg.lookup(name)
	if !ok {
		t.Fatalf("%s not registered", name)
	}

	return fn(context.Background(), a)
}

func TestHandleBindsPositionalArgumentsOfAnyType(t *testing.T) {
	reg := NewRegistry()
	reg.Handle("Orders.make", func(ctx context.Context, id int, note string, lines []tLine) (tOrder, error) {
		if ctx == nil {
			t.Fatal("no context")
		}

		return tOrder{ID: id, Note: note, Lines: lines}, nil
	})

	got, err := call(t, reg, "Orders.make", args(t, 7, "rush", []tLine{{SKU: "a", Qty: 2}}))
	if err != nil {
		t.Fatal(err)
	}

	order := got.(tOrder)
	if order.ID != 7 || order.Note != "rush" || order.Lines[0].Qty != 2 {
		t.Fatalf("got %+v", order)
	}
}

func TestATrailingPointerMayBeLeftOutAndTheRestIsVariadic(t *testing.T) {
	reg := NewRegistry()
	reg.Handle("Orders.find", func(id int, opts *tFind) (bool, error) {
		return opts != nil && opts.IncludeLines, nil
	})
	reg.Handle("Tags.join", func(sep string, tags ...string) string {
		return strings.Join(tags, sep)
	})

	if got, _ := call(t, reg, "Orders.find", args(t, 1)); got != false {
		t.Fatalf("left out: %v", got)
	}

	if got, _ := call(t, reg, "Orders.find", args(t, 1, tFind{IncludeLines: true})); got != true {
		t.Fatalf("passed: %v", got)
	}

	if got, _ := call(t, reg, "Tags.join", args(t, "-", "a", "b", "c")); got != "a-b-c" {
		t.Fatalf("variadic: %v", got)
	}

	if _, err := call(t, reg, "Orders.find", Args{}); err == nil {
		t.Fatal("a required argument left out was accepted")
	}
}

func TestAnErrorIsReturnedAsTheCallsError(t *testing.T) {
	reg := NewRegistry()
	reg.Handle("Orders.cancel", func(ctx context.Context, id int) error { return Refuse(409, "already shipped") })

	_, err := call(t, reg, "Orders.cancel", args(t, 1))

	var refusal *RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v", err)
	}
}

func TestARefusalCarriesItsDataOnTheWire(t *testing.T) {
	type blocker struct {
		ID   int    `json:"id"`
		Href string `json:"href"`
	}

	status, reply := replyFor(RefuseWith(0, "Still in use", map[string]any{"blockers": []blocker{{7, "/orders/7"}}}))

	if status != http.StatusConflict || reply.RefusalStatus != http.StatusConflict {
		t.Fatalf("status = %d, refusalStatus = %d, want 409", status, reply.RefusalStatus)
	}

	body, _ := json.Marshal(reply)

	if want := `{"error":"Still in use","refusalStatus":409,"refusalData":{"blockers":[{"id":7,"href":"/orders/7"}]}}`; string(body) != want {
		t.Fatalf("reply = %s\nwant %s", body, want)
	}

	// Without data, the reply is the one Refuse always sent.
	_, plain := replyFor(Refuse(429, "Slow down."))
	body, _ = json.Marshal(plain)

	if want := `{"error":"Slow down.","refusalStatus":429}`; string(body) != want {
		t.Fatalf("reply = %s\nwant %s", body, want)
	}
}

func TestTheManifestDescribesEachSignature(t *testing.T) {
	reg := NewRegistry()
	reg.Handle("Orders.recent", func(ctx context.Context, limit int) ([]tOrder, error) { return nil, nil })
	reg.HandleAction("ordersFind", "Orders.find", func(id int, opts *tFind) (tOrder, error) { return tOrder{}, nil })
	reg.Handle("Tags.join", func(sep string, tags ...string) string { return "" })
	reg.Register("Legacy.read", func(context.Context, Args) (any, error) { return nil, nil })

	path := filepath.Join(t.TempDir(), "rsc-host.json")
	if err := reg.WriteManifest(path); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)

	var m struct {
		Actions   map[string]string         `json:"actions"`
		Functions []string                  `json:"functions"`
		Types     map[string]map[string]any `json:"types"`
		Defs      map[string]map[string]any `json:"defs"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s", raw)
	}

	if _, typed := m.Types["Legacy.read"]; typed {
		t.Fatal("a Register function was given a type")
	}

	recent := m.Types["Orders.recent"]
	want := map[string]any{
		"params": []any{map[string]any{"type": "integer"}},
		"result": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/defs/tOrder"}},
	}
	if !reflect.DeepEqual(recent, want) {
		t.Fatalf("Orders.recent = %v", recent)
	}

	if m.Types["Orders.find"]["optional"] != float64(1) {
		t.Fatalf("Orders.find = %v", m.Types["Orders.find"])
	}

	if !reflect.DeepEqual(m.Types["Tags.join"]["rest"], map[string]any{"type": "string"}) {
		t.Fatalf("Tags.join = %v", m.Types["Tags.join"])
	}

	order := m.Defs["tOrder"]
	props := order["properties"].(map[string]any)

	for _, name := range []string{"Secret", "private"} {
		if _, ok := props[name]; ok {
			t.Fatalf("%s is in the schema", name)
		}
	}

	if !reflect.DeepEqual(props["placed"], map[string]any{"type": "string", "format": "date-time"}) {
		t.Fatalf("placed = %v", props["placed"])
	}

	// A type that refers to itself is one definition, referred to.
	parent := props["parent"].(map[string]any)["anyOf"].([]any)[0]
	if !reflect.DeepEqual(parent, map[string]any{"$ref": "#/defs/tOrder"}) {
		t.Fatalf("parent = %v", parent)
	}

	required := order["required"].([]any)
	for _, name := range required {
		if name == "note" {
			t.Fatal("an omitempty field is required")
		}
	}
}

func TestHandleRefusesWhatIsNotAFunction(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("did not panic")
		}
	}()

	NewRegistry().Handle("Nope", 42)
}
