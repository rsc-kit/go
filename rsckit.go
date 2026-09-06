// Package rsckit lets a Go server host an rsc-kit application.
//
// The division of labour is the point. The JS process renders — it owns
// routing, the header protocol, partial navigation, prerendered variants and
// PPR, all of which are subtle and all of which @rsc-kit/core/host already
// implements. Go owns the request: sessions, auth, the database. A server
// component reaches Go by calling rpc(), which arrives here as an ordinary
// POST.
//
// That is the whole adapter. There is no frame protocol to implement, because
// the thing that made one necessary — a callback channel over a raw socket —
// is an HTTP endpoint instead.
//
//	reg := rsckit.NewRegistry()
//	reg.Register("Orders.recent", func(ctx context.Context, args rsckit.Args) (any, error) {
//	    var limit int
//	    if err := args.Bind(&limit); err != nil {
//	        return nil, err
//	    }
//	    return db.RecentOrders(ctx, limit)
//	})
package rsckit

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// SecretHeader carries the shared secret on every host call. The JS side sends
// it from httpHostCalls; this side refuses anything without it.
const SecretHeader = "X-Rsc-Host-Secret"

// Args are the arguments a server component passed to rpc(), still encoded.
//
// They stay as raw JSON until a function says what it wants them to be, so a
// registry can hold functions of different shapes without reflection.
type Args []json.RawMessage

// Bind decodes positional arguments into the given pointers.
//
// Extra arguments are ignored — a component passing more than the function
// reads is not an error worth failing a render over. Missing ones are, because
// the alternative is a zero value that looks like data.
func (a Args) Bind(targets ...any) error {
	if len(targets) > len(a) {
		return fmt.Errorf("host call wants %d argument(s), got %d", len(targets), len(a))
	}

	for i, target := range targets {
		if err := json.Unmarshal(a[i], target); err != nil {
			return fmt.Errorf("argument %d: %w", i, err)
		}
	}

	return nil
}

// Len reports how many arguments were passed.
func (a Args) Len() int { return len(a) }

// Func is a function a server component can call.
type Func func(ctx context.Context, args Args) (any, error)

// Registry holds the functions this host answers.
//
// Safe for concurrent use: registration usually happens at startup, but a
// render calls into it from whatever goroutine is serving the callback, and
// several renders are in flight at once.
type Registry struct {
	mu  sync.RWMutex
	fns map[string]Func
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{fns: make(map[string]Func)}
}

// Register adds a function under the name server components call it by.
//
// Registering the same name twice panics rather than overwriting. A silent
// overwrite is the kind of thing that survives a refactor and then answers the
// wrong query in production.
func (r *Registry) Register(name string, fn Func) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.fns[name]; exists {
		panic(fmt.Sprintf("rsckit: host function %q registered twice", name))
	}

	r.fns[name] = fn
}

// Names lists what is registered, for the build's RSC_HOST_ACTIONS and for
// diagnostics.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.fns))
	for name := range r.fns {
		names = append(names, name)
	}

	return names
}

func (r *Registry) lookup(name string) (Func, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	fn, ok := r.fns[name]

	return fn, ok
}

type ctxKey int

const (
	headerKey ctxKey = iota
	revalidateKey
)

// HeadersFrom returns the render request's forwarded headers — the cookie and
// authorization of the person the page is being rendered for.
//
// This is what makes a host call run as that visitor rather than as nobody:
// pass these to whatever reads a session and the answer is theirs. Empty
// during a build-time render, which has no visitor and should not have one.
func HeadersFrom(ctx context.Context) http.Header {
	if h, ok := ctx.Value(headerKey).(http.Header); ok {
		return h
	}

	return http.Header{}
}

// Revalidate marks a region as stale, so the answer to an action can carry the
// re-rendered parts instead of telling the browser to ask again.
func Revalidate(ctx context.Context, targets ...string) {
	if box, ok := ctx.Value(revalidateKey).(*revalidations); ok {
		box.add(targets...)
	}
}

type revalidations struct {
	mu      sync.Mutex
	targets []string
}

func (r *revalidations) add(targets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = append(r.targets, targets...)
}

func (r *revalidations) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.targets
}

type callRequest struct {
	Function string            `json:"function"`
	Args     []json.RawMessage `json:"args"`
}

type callReply struct {
	Result           any                 `json:"result,omitempty"`
	Error            string              `json:"error,omitempty"`
	Revalidate       []string            `json:"revalidate,omitempty"`
	ValidationErrors map[string][]string `json:"validationErrors,omitempty"`
}

// ValidationError refuses the input, naming the fields and what is wrong with
// each. Return it from a host function — `return nil, rsckit.Invalid(...)` —
// and the form shows each message under its own input.
//
// It is not the call failing. A failure is a 500 the visitor should never
// cause; this is the ordinary answer to a form that was filled in wrongly, and
// it travels as its own field so the two are never confused.
type ValidationError struct {
	Errors map[string][]string
}

func (e *ValidationError) Error() string {
	fields := make([]string, 0, len(e.Errors))
	for field := range e.Errors {
		fields = append(fields, field)
	}

	sort.Strings(fields)

	return "invalid input: " + strings.Join(fields, ", ")
}

// Invalid builds a refusal from field names to messages.
//
// The shape is deliberately the one every host here already speaks — Laravel's
// own $e->errors(), the socket protocol's validation_errors, and what a
// Standard Schema result is converted into. Dot-joined for a nested field
// ("address.city"), the empty string for a message about the form rather than
// any one field.
func Invalid(errors map[string][]string) error {
	return &ValidationError{Errors: errors}
}

// InvalidField is the single-field case, which is most of them.
func InvalidField(field string, messages ...string) error {
	return &ValidationError{Errors: map[string][]string{field: messages}}
}

// ErrNoSecret is returned by NewCallbackHandler when built without one.
var ErrNoSecret = errors.New("rsckit: a callback handler needs a shared secret")

// CallbackHandler answers host calls from the JS renderer.
//
// Mount it where only the renderer can reach it, and give it the same secret
// httpHostCalls was given. It is not a public endpoint: it runs functions by
// name, and nothing in front of it is doing the app's routing or authorization.
type CallbackHandler struct {
	registry *Registry
	secret   []byte

	// ForwardHeaders are copied from the call onto the context a function
	// sees. Defaults to cookie and authorization, matching the JS side.
	ForwardHeaders []string
}

// NewCallbackHandler builds the endpoint the renderer calls back into.
func NewCallbackHandler(registry *Registry, secret string) (*CallbackHandler, error) {
	if secret == "" {
		return nil, ErrNoSecret
	}

	return &CallbackHandler{
		registry:       registry,
		secret:         []byte(secret),
		ForwardHeaders: []string{"Cookie", "Authorization"},
	}, nil
}

func (h *CallbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeReply(w, http.StatusMethodNotAllowed, callReply{Error: "host calls are POST"})

		return
	}

	// ConstantTimeCompare rather than ==: this is a secret being checked on a
	// network endpoint, and its length is already known to anyone who looks at
	// the config.
	given := []byte(r.Header.Get(SecretHeader))
	if subtle.ConstantTimeCompare(given, h.secret) != 1 {
		writeReply(w, http.StatusForbidden, callReply{Error: "bad or missing host secret"})

		return
	}

	var call callRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&call); err != nil {
		writeReply(w, http.StatusBadRequest, callReply{Error: "malformed host call: " + err.Error()})

		return
	}

	fn, ok := h.registry.lookup(call.Function)
	if !ok {
		// Named, because the JS side deliberately cannot say which function is
		// missing — it does not know what this host registered.
		writeReply(w, http.StatusNotFound, callReply{
			Error: fmt.Sprintf("no host function named %q; registered: %v", call.Function, h.registry.Names()),
		})

		return
	}

	forwarded := http.Header{}
	for _, name := range h.ForwardHeaders {
		if v := r.Header.Get(name); v != "" {
			forwarded.Set(name, v)
		}
	}

	box := &revalidations{}
	ctx := context.WithValue(r.Context(), headerKey, forwarded)
	ctx = context.WithValue(ctx, revalidateKey, box)

	result, err := h.call(ctx, fn, call.Args)

	// A refusal is an answer, not a failure. 422 rather than 500, and the
	// fields travel in their own key so the renderer can tell one from the
	// other without parsing a message.
	var invalid *ValidationError

	if errors.As(err, &invalid) {
		writeReply(w, http.StatusUnprocessableEntity, callReply{ValidationErrors: invalid.Errors})

		return
	}

	if err != nil {
		writeReply(w, http.StatusInternalServerError, callReply{Error: err.Error()})

		return
	}

	writeReply(w, http.StatusOK, callReply{Result: result, Revalidate: box.all()})
}

// call runs the function, turning a panic into an error.
//
// A panicking host function would otherwise take down the whole server, and
// with it every other render in flight — for what is, from the renderer's
// point of view, one component failing to fetch.
func (h *CallbackHandler) call(ctx context.Context, fn Func, args Args) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("host function panicked: %v", r)
		}
	}()

	return fn(ctx, args)
}

func writeReply(w http.ResponseWriter, status int, reply callReply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(reply)
}
