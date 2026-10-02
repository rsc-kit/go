package rsckit

import (
	"context"
	"encoding/json"
	"errors"
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
