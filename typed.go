package rsckit

import (
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Schema is a JSON Schema, the subset the build turns into TypeScript.
type Schema = map[string]any

// Signature is what a typed function takes and returns, as the build reads it
// from rsc-host.json: one schema per positional parameter, in order, and the
// result's.
type Signature struct {
	// Params are the parameters after the context, in order.
	Params []Schema `json:"params"`
	// Optional is how many trailing parameters may be left out: a trailing
	// pointer is nil when the caller passed nothing for it.
	Optional int `json:"optional,omitempty"`
	// Rest is the element schema of a variadic final parameter.
	Rest Schema `json:"rest,omitempty"`
	// Result is what the function returns; absent when it returns only an
	// error.
	Result Schema `json:"result,omitempty"`
}

var (
	contextType = reflect.TypeFor[context.Context]()
	errorType   = reflect.TypeFor[error]()
	timeType    = reflect.TypeFor[time.Time]()
	rawType     = reflect.TypeFor[json.RawMessage]()
	marshaler   = reflect.TypeFor[json.Marshaler]()
	textMarsh   = reflect.TypeFor[encoding.TextMarshaler]()
)

// Handle registers an ordinary Go function under name, typed.
//
//	reg.Handle("Orders.recent", func(ctx context.Context, limit int) ([]Order, error) { … })
//	reg.Handle("Orders.find", func(id int, opts *FindOptions) (Order, error) { … })
//
// Any number of parameters, of any type JSON can carry, bound from rpc()'s
// arguments in order. A leading context.Context is the call's. It returns a
// value and an error, only an error, or only a value. A trailing pointer
// parameter may be left out by the caller and is nil; a variadic one takes
// the rest.
//
// The parameter and result types are written to rsc-host.json, so the app's
// rpc('Orders.recent', 5) is typed as returning Order[] and rejects a string.
// It panics on a value that is not such a function, at startup, where the
// mistake is.
func (r *Registry) Handle(name string, fn any) {
	call, sig := typedFunc(name, fn, r.defs())
	r.Register(name, call)

	r.mu.Lock()
	r.sigs[name] = sig
	r.mu.Unlock()
}

// HandleAction is Handle for a server action, exported to the app as jsName.
// A form posting to it sends its fields as the first parameter.
func (r *Registry) HandleAction(jsName, name string, fn any) {
	call, sig := typedFunc(name, fn, r.defs())
	r.RegisterAction(jsName, name, call)

	r.mu.Lock()
	r.sigs[name] = sig
	r.mu.Unlock()
}

func (r *Registry) defs() *defs {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.typeDefs == nil {
		r.typeDefs = &defs{byType: map[reflect.Type]string{}, schemas: map[string]Schema{}}
	}

	return r.typeDefs
}

func typedFunc(name string, fn any, d *defs) (Func, Signature) {
	v := reflect.ValueOf(fn)
	t := v.Type()

	if t.Kind() != reflect.Func {
		panic(fmt.Sprintf("rsckit: %q is a %s, not a function", name, t))
	}

	first := 0
	if t.NumIn() > 0 && t.In(0) == contextType {
		first = 1
	}

	var result reflect.Type

	switch {
	case t.NumOut() == 2 && t.Out(1) == errorType:
		result = t.Out(0)
	case t.NumOut() == 1 && t.Out(0) == errorType:
	case t.NumOut() == 1:
		result = t.Out(0)
	case t.NumOut() == 0:
	default:
		panic(fmt.Sprintf("rsckit: %q returns %d values; return a value and an error, or one of them", name, t.NumOut()))
	}

	params := make([]reflect.Type, 0, t.NumIn()-first)
	for i := first; i < t.NumIn(); i++ {
		params = append(params, t.In(i))
	}

	sig := Signature{Params: []Schema{}}
	fixed := params

	if t.IsVariadic() {
		fixed = params[:len(params)-1]
		sig.Rest = d.of(params[len(params)-1].Elem())
	}

	for _, p := range fixed {
		sig.Params = append(sig.Params, d.of(p))
	}

	for i := len(fixed) - 1; i >= 0 && fixed[i].Kind() == reflect.Pointer; i-- {
		sig.Optional++
	}

	if result != nil {
		sig.Result = d.of(result)
	}

	call := func(ctx context.Context, args Args) (any, error) {
		in := make([]reflect.Value, 0, t.NumIn())

		if first == 1 {
			in = append(in, reflect.ValueOf(ctx))
		}

		required := len(fixed) - sig.Optional

		if len(args) < required {
			return nil, fmt.Errorf("host call %q wants %d argument(s), got %d", name, required, len(args))
		}

		for i, p := range fixed {
			target := reflect.New(p)

			if i < len(args) {
				if err := json.Unmarshal(args[i], target.Interface()); err != nil {
					return nil, fmt.Errorf("argument %d: %w", i, err)
				}
			}

			in = append(in, target.Elem())
		}

		if t.IsVariadic() {
			elem := params[len(params)-1].Elem()

			for i := len(fixed); i < len(args); i++ {
				target := reflect.New(elem)

				if err := json.Unmarshal(args[i], target.Interface()); err != nil {
					return nil, fmt.Errorf("argument %d: %w", i, err)
				}

				in = append(in, target.Elem())
			}
		}

		out := v.Call(in)

		switch {
		case t.NumOut() == 2:
			err, _ := out[1].Interface().(error)

			return out[0].Interface(), err
		case t.NumOut() == 1 && t.Out(0) == errorType:
			err, _ := out[0].Interface().(error)

			return nil, err
		case t.NumOut() == 1:
			return out[0].Interface(), nil
		default:
			return nil, nil
		}
	}

	return call, sig
}

// defs names the struct types a registry's signatures refer to, once each,
// so the build writes one interface per Go type rather than repeating it
// wherever it appears - and a type that refers to itself terminates.
type defs struct {
	byType  map[reflect.Type]string
	schemas map[string]Schema
}

func (d *defs) of(t reflect.Type) Schema {
	// A pointer is its element or null, decided before asking what the type
	// marshals as: *time.Time has time.Time's MarshalJSON too, and was read
	// as a type whose shape is its own - unknown, where it is a string or null.
	if t.Kind() == reflect.Pointer {
		return Schema{"anyOf": []any{d.of(t.Elem()), Schema{"type": "null"}}}
	}

	if t == timeType {
		return Schema{"type": "string", "format": "date-time"}
	}

	if t == rawType {
		return Schema{}
	}

	// A type that writes its own JSON says nothing about its shape.
	if t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler) {
		return Schema{}
	}

	if t.Implements(textMarsh) || reflect.PointerTo(t).Implements(textMarsh) {
		return Schema{"type": "string"}
	}

	switch t.Kind() {
	case reflect.Bool:
		return Schema{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return Schema{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return Schema{"type": "number"}
	case reflect.String:
		return Schema{"type": "string"}
	case reflect.Slice, reflect.Array:
		// []byte is base64 text on the wire.
		if t.Elem().Kind() == reflect.Uint8 {
			return Schema{"type": "string"}
		}

		return Schema{"type": "array", "items": d.of(t.Elem())}
	case reflect.Map:
		return Schema{"type": "object", "additionalProperties": d.of(t.Elem())}
	case reflect.Struct:
		return d.object(t)
	default:
		// any, an interface, a channel: whatever arrives.
		return Schema{}
	}
}

func (d *defs) object(t reflect.Type) Schema {
	if t.Name() == "" {
		return d.fields(t)
	}

	if name, ok := d.byType[t]; ok {
		return Schema{"$ref": "#/defs/" + name}
	}

	name := t.Name()

	// Two packages' Order are two types: the second is qualified.
	if _, taken := d.schemas[name]; taken {
		name = exported(pathTail(t.PkgPath())) + name
	}

	d.byType[t] = name
	d.schemas[name] = Schema{}
	d.schemas[name] = d.fields(t)

	return Schema{"$ref": "#/defs/" + name}
}

func (d *defs) fields(t reflect.Type) Schema {
	properties := Schema{}
	required := []string{}

	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() || f.Anonymous && f.Type.Kind() == reflect.Struct && f.Tag.Get("json") == "" {
			continue
		}

		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}

		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}

		schema := d.of(f.Type)
		if strings.Contains(opts, "string") {
			schema = Schema{"type": "string"}
		}

		properties[name] = schema

		if !strings.Contains(opts, "omitempty") && !strings.Contains(opts, "omitzero") {
			required = append(required, name)
		}
	}

	sort.Strings(required)

	out := Schema{"type": "object", "properties": properties}
	if len(required) > 0 {
		out["required"] = required
	}

	return out
}

func pathTail(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}

	return path
}

func exported(s string) string {
	if s == "" {
		return s
	}

	return strings.ToUpper(s[:1]) + s[1:]
}
