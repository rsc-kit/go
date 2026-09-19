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
	"os"
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

// Guard is a route middleware: something that must hold before anything at
// or below a directory renders.
//
// A route.ts names guards in this host's vocabulary, and the renderer asks
// for them by name before rendering - including before serving a page it
// froze at build time. The param is what follows the colon in the name:
// "can:manage-users" reaches the guard registered as "can" with
// "manage-users"; "throttle:60,1" with "60,1"; "auth" with "".
//
// Return nil to let the render go ahead. Anything else refuses it, and the
// error decides how: Unauthenticated answers 401, Redirect sends the visitor
// to sign in, Refuse(429, ...) keeps a throttle's own status, and an
// ordinary error is a 500. There is no way to refuse quietly, on purpose.
type Guard func(ctx context.Context, param string) error

// MiddlewareFunction is the reserved name the renderer asks route guards on.
// It is answered by the registry itself, never registered by an app.
const MiddlewareFunction = "__rsc.middleware"

// Registry holds the functions this host answers.
//
// Safe for concurrent use: registration usually happens at startup, but a
// render calls into it from whatever goroutine is serving the callback, and
// several renders are in flight at once.
type Registry struct {
	mu      sync.RWMutex
	fns     map[string]Func
	guards  map[string]Guard
	actions map[string]string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		fns:     make(map[string]Func),
		guards:  make(map[string]Guard),
		actions: make(map[string]string),
	}
}

// Middleware registers a guard under the name a route.ts uses for it.
//
// Registering a name twice panics, for the reason Register does.
func (r *Registry) Middleware(name string, guard Guard) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.guards[name]; exists {
		panic(fmt.Sprintf("rsckit: middleware %q registered twice", name))
	}

	r.guards[name] = guard
}

// RegisterAction registers a function the browser may call as a server
// action, under jsName in the app's code.
//
// The build reads the map from rsc-host-actions.json and writes a "use server"
// module exporting jsName; a client component imports it and calls it, and
// the call arrives here under name. WriteActionManifest writes that file.
func (r *Registry) RegisterAction(jsName, name string, fn Func) {
	r.Register(name, fn)

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.actions[jsName]; exists {
		panic(fmt.Sprintf("rsckit: action %q registered twice", jsName))
	}

	r.actions[jsName] = name
}

// ActionManifest is what the build reads: the JavaScript name of each action
// to the name it is registered under here.
func (r *Registry) ActionManifest() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]string, len(r.actions))
	for js, name := range r.actions {
		out[js] = name
	}

	return out
}

// WriteActionManifest writes rsc-host-actions.json where the build looks for
// it - the project root, beside vite.config.ts.
//
// Run it before each build rather than by hand: a stale map names a function
// that has since been renamed, and nothing fails until the browser calls it.
// A registry with no actions writes an empty object, so a removed action
// disappears from the generated module rather than lingering.
func (r *Registry) WriteActionManifest(path string) error {
	data, err := json.MarshalIndent(r.ActionManifest(), "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// runGuards answers the renderer's reserved call: every guard named, in
// order, outermost first, stopping at the first refusal - an outer guard
// saying no means the inner one should never have been asked.
//
// A name nothing here answers to is a refusal, not a pass. A route that
// declares a guard this host does not have is a check that silently does not
// happen, and the only safe reading of that is no.
func (r *Registry) runGuards(ctx context.Context, args Args) (any, error) {
	var names []string
	if err := args.Bind(&names); err != nil {
		return nil, err
	}

	for _, full := range names {
		name, param, _ := strings.Cut(full, ":")

		r.mu.RLock()
		guard, ok := r.guards[name]
		r.mu.RUnlock()

		if !ok {
			return nil, fmt.Errorf("route middleware %q is declared and this host has no guard named %q; registered: %v",
				full, name, r.GuardNames())
		}

		if err := guard(ctx, param); err != nil {
			return nil, err
		}
	}

	return true, nil
}

// GuardNames lists the middleware this host answers to.
func (r *Registry) GuardNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.guards))
	for name := range r.guards {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// Register adds a function under the name server components call it by.
//
// Registering the same name twice panics rather than overwriting. A silent
// overwrite is the kind of thing that survives a refactor and then answers the
// wrong query in production.
func (r *Registry) Register(name string, fn Func) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if name == MiddlewareFunction {
		panic(fmt.Sprintf("rsckit: %q is reserved; register guards with Middleware", name))
	}

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

	sort.Strings(names)

	return names
}

func (r *Registry) lookup(name string) (Func, bool) {
	if name == MiddlewareFunction {
		return r.runGuards, true
	}

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
	// A batch: several calls the renderer issued in one tick of a render,
	// answered in order. Set instead of Function.
	Calls []callRequest `json:"calls"`
}

// batchItem is one answer inside a batch, carrying the status the call
// would have had on its own.
type batchItem struct {
	Status int `json:"status"`
	callReply
}

type batchReply struct {
	Replies []batchItem `json:"replies"`
}

// callReply is the wire shape, the same one Laravel answers with. Every
// outcome has its own field so the renderer never reads a message to tell an
// invalid form from a broken server, or a redirect from a result.
type callReply struct {
	Result           any                 `json:"result,omitempty"`
	Error            string              `json:"error,omitempty"`
	Revalidate       []string            `json:"revalidate,omitempty"`
	ValidationErrors map[string][]string `json:"validationErrors,omitempty"`
	Unauthenticated  bool                `json:"unauthenticated,omitempty"`
	Unauthorized     bool                `json:"unauthorized,omitempty"`
	Redirect         string              `json:"redirect,omitempty"`
	RedirectStatus   int                 `json:"redirectStatus,omitempty"`
	RefusalStatus    int                 `json:"refusalStatus,omitempty"`
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

// AuthenticationError says the caller has no session. The render answers 401,
// the way it would if a JavaScript guard had thrown ServerAuthenticationError.
type AuthenticationError struct{ Message string }

func (e *AuthenticationError) Error() string { return e.Message }

// Unauthenticated refuses a call from nobody. The message is optional.
func Unauthenticated(message ...string) error {
	return &AuthenticationError{Message: first(message, "Unauthenticated.")}
}

// AuthorizationError says the caller has a session and still may not. 403.
type AuthorizationError struct{ Message string }

func (e *AuthorizationError) Error() string { return e.Message }

// Unauthorized refuses a call from someone who is signed in and not allowed.
func Unauthorized(message ...string) error {
	return &AuthorizationError{Message: first(message, "This action is unauthorized.")}
}

// RedirectError sends the visitor somewhere else - to sign in, usually.
//
// It travels as a 200 with the destination in the body, never as a 3xx: an
// HTTP client follows a redirect transparently, so a real one would send the
// host call itself to the destination and hand whatever it found back to the
// render as the function's result.
type RedirectError struct {
	Location string
	// Status the browser is redirected with. 0 means 307, which keeps the
	// method - a POSTed form stays a POST if it is redirected somewhere that
	// expects one.
	Status int
}

func (e *RedirectError) Error() string { return "redirect to " + e.Location }

// Redirect answers a call by sending the visitor to location instead.
func Redirect(location string, status ...int) error {
	return &RedirectError{Location: location, Status: firstInt(status, 0)}
}

// RefusalError refuses with a status the guard chose - a throttle's 429, a
// signed-url check's 403 - rather than the 500 an ordinary error becomes.
// Collapsing them makes a rate-limited visitor indistinguishable from a broken
// server, in the logs and to the person looking at it.
type RefusalError struct {
	Status  int
	Message string
}

func (e *RefusalError) Error() string { return e.Message }

// Refuse answers with status, and says why.
func Refuse(status int, message string) error {
	if message == "" {
		message = http.StatusText(status)
	}

	return &RefusalError{Status: status, Message: message}
}

func first(values []string, fallback string) string {
	if len(values) > 0 && values[0] != "" {
		return values[0]
	}

	return fallback
}

func firstInt(values []int, fallback int) int {
	if len(values) > 0 && values[0] != 0 {
		return values[0]
	}

	return fallback
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

	forwarded := http.Header{}
	for _, name := range h.ForwardHeaders {
		if v := r.Header.Get(name); v != "" {
			forwarded.Set(name, v)
		}
	}

	ctx := context.WithValue(r.Context(), headerKey, forwarded)

	// A batch: one HTTP request for a page's parallel reads rather than one
	// each. Every call is answered, in order, as it would have been alone -
	// a refusal in the third is that call's answer, not a reason to leave the
	// fourth unanswered.
	if call.Calls != nil {
		if len(call.Calls) == 0 {
			writeReply(w, http.StatusBadRequest, callReply{Error: "a batch needs a non-empty \"calls\" list"})

			return
		}

		items := make([]batchItem, 0, len(call.Calls))
		for _, one := range call.Calls {
			status, reply := h.dispatch(ctx, one)
			items = append(items, batchItem{Status: status, callReply: reply})
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(batchReply{Replies: items})

		return
	}

	status, reply := h.dispatch(ctx, call)
	writeReply(w, status, reply)
}

// dispatch runs one call and decides its answer. Each call gets its own
// revalidation box, so what one marked stale never rides on another's reply.
func (h *CallbackHandler) dispatch(ctx context.Context, call callRequest) (int, callReply) {
	fn, ok := h.registry.lookup(call.Function)
	if !ok {
		// Named, because the JS side deliberately cannot say which function is
		// missing — it does not know what this host registered.
		return http.StatusNotFound, callReply{
			Error: fmt.Sprintf("no host function named %q; registered: %v", call.Function, h.registry.Names()),
		}
	}

	box := &revalidations{}
	ctx = context.WithValue(ctx, revalidateKey, box)

	result, err := h.call(ctx, fn, call.Args)
	if err != nil {
		return replyFor(err)
	}

	return http.StatusOK, callReply{Result: result, Revalidate: box.all()}
}

// replyFor turns what a function returned into the answer on the wire.
//
// A refusal is an answer, not a failure: each kind has its own status and its
// own field, so the renderer can tell them apart without parsing a message.
// Only an error that is none of these is the 500 the visitor did not cause.
func replyFor(err error) (int, callReply) {
	var (
		invalid  *ValidationError
		noone    *AuthenticationError
		mayNot   *AuthorizationError
		redirect *RedirectError
		refused  *RefusalError
	)

	switch {
	case errors.As(err, &invalid):
		return http.StatusUnprocessableEntity, callReply{ValidationErrors: invalid.Errors}
	case errors.As(err, &noone):
		return http.StatusUnauthorized, callReply{Unauthenticated: true, Error: noone.Message}
	case errors.As(err, &mayNot):
		return http.StatusForbidden, callReply{Unauthorized: true, Error: mayNot.Message}
	case errors.As(err, &redirect):
		return http.StatusOK, callReply{Redirect: redirect.Location, RedirectStatus: redirect.Status}
	case errors.As(err, &refused):
		return refused.Status, callReply{Error: refused.Message, RefusalStatus: refused.Status}
	}

	return http.StatusInternalServerError, callReply{Error: err.Error()}
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
