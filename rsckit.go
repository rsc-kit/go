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
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// sigs are the types of what Handle registered; typeDefs the named
	// struct types they refer to.
	sigs     map[string]Signature
	typeDefs *defs

	// Versions a page refreshes on - see changed.go. moved is closed by each
	// Changed in this process, waking a ChangedFunction call being held.
	versionsMu sync.Mutex
	versions   VersionStore
	moved      chan struct{}
	// listening is true while a WakeOn listener is connected: a held
	// ChangedFunction call then waits for it rather than reading the store
	// every ChangedPoll.
	listening atomic.Bool
	// waiters are the ChangedFunction calls held here; polling is whether
	// the one poller that answers them all is running.
	waiters map[*waiter]struct{}
	polling bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		fns:     make(map[string]Func),
		guards:  make(map[string]Guard),
		actions: make(map[string]string),
		sigs:    make(map[string]Signature),
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
// The build reads it from rsc-host.json and writes a "use server" module
// exporting jsName; a client component imports it and calls it, and the call
// arrives here under name. WriteManifest writes that file.
func (r *Registry) RegisterAction(jsName, name string, fn Func) {
	r.Register(name, fn)

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.actions[jsName]; exists {
		panic(fmt.Sprintf("rsckit: action %q registered twice", jsName))
	}

	r.actions[jsName] = name
}

// Manifest is what the build reads from rsc-host.json.
type Manifest struct {
	// Actions maps each action's JavaScript name to the name it is
	// registered under; the build writes a "use server" stub for each.
	Actions map[string]string `json:"actions"`
	// Functions is every name rpc() may call, sorted; the build turns it
	// into a type, so a misspelt name fails the typecheck.
	Functions []string `json:"functions"`
	// Types are the signatures of the functions registered with Handle, by
	// name; the build types rpc() and the action stubs from them. A function
	// registered with Register has none and stays untyped.
	Types map[string]Signature `json:"types,omitempty"`
	// Defs are the named struct types those signatures refer to, by
	// "$ref": "#/defs/Name". One TypeScript interface each.
	Defs map[string]Schema `json:"defs,omitempty"`
}

// Manifest lists what this registry offers the app.
func (r *Registry) Manifest() Manifest {
	r.mu.RLock()
	actions := make(map[string]string, len(r.actions))
	for js, name := range r.actions {
		actions[js] = name
	}
	var types map[string]Signature
	if len(r.sigs) > 0 {
		types = make(map[string]Signature, len(r.sigs))
		for name, sig := range r.sigs {
			types[name] = sig
		}
	}

	var named map[string]Schema
	if r.typeDefs != nil && len(r.typeDefs.schemas) > 0 {
		named = make(map[string]Schema, len(r.typeDefs.schemas))
		for name, schema := range r.typeDefs.schemas {
			named[name] = schema
		}
	}
	r.mu.RUnlock()

	return Manifest{Actions: actions, Functions: r.Names(), Types: types, Defs: named}
}

// WriteManifest writes rsc-host.json where the build looks for it - the
// project root, beside vite.config.ts.
//
// Run it before each build and dev start - rscKit({ hostManifest }) does - so
// it cannot go stale: a stale one names a function since renamed, and nothing
// fails until the browser calls it. A registry with no actions writes an
// empty object, so a removed action disappears from the generated module.
func (r *Registry) WriteManifest(path string) error {
	data, err := json.MarshalIndent(r.Manifest(), "", "  ")
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

	if name == ChangedFunction {
		panic(fmt.Sprintf("rsckit: %q is reserved; it is answered by the registry, and Changed moves a version", name))
	}

	if _, exists := r.fns[name]; exists {
		panic(fmt.Sprintf("rsckit: host function %q registered twice", name))
	}

	r.fns[name] = fn
}

// Names lists what is registered, sorted: the manifest's functions.
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

	if name == ChangedFunction {
		return r.runChanged, true
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
	// A copy: the calls in a batch run concurrently and share one set, so a
	// function that set a header on what it was handed raced its siblings.
	if h, ok := ctx.Value(headerKey).(http.Header); ok {
		return h.Clone()
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

	// A copy, so a Revalidate from a goroutine that outlived its function
	// cannot race the reply being written.
	return append([]string(nil), r.targets...)
}

type callRequest struct {
	Function string            `json:"function"`
	Args     []json.RawMessage `json:"args"`
	// A batch: several calls the renderer issued in one tick of a render,
	// run concurrently and answered as each finishes. Set instead of Function.
	Calls []callRequest `json:"calls"`
}

// batchLine is one answer inside a batch, written the moment its call has
// finished: the call's position in the batch, the status it would have had
// on its own, and the reply. One JSON line each, in order of completion.
type batchLine struct {
	Index  int `json:"index"`
	Status int `json:"status"`
	callReply
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
	// Debug says where an unexpected failure happened, for the renderer to
	// show beside its own stack. Only when the handler's Debug is on.
	Debug *debugInfo `json:"debug,omitempty"`
}

type debugInfo struct {
	Type    string   `json:"type"`
	Message string   `json:"message"`
	Trace   []string `json:"trace"`
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

	// Debug sends where an unexpected failure happened with its answer: the
	// error's type, and for a panic the frames it unwound, so the renderer's
	// error points at the Go that failed rather than only at the rpc() call.
	// Development only - a trace names files and functions. Defaults to
	// RSC_DEBUG=1 in the environment.
	Debug bool
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
		Debug:          os.Getenv("RSC_DEBUG") == "1",
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
	// Checked before the comparison, not left to it: ConstantTimeCompare of
	// two empty values is a match, so a handler built without
	// NewCallbackHandler would admit a request that carried no secret.
	given := []byte(r.Header.Get(SecretHeader))
	if len(h.secret) == 0 || subtle.ConstantTimeCompare(given, h.secret) != 1 {
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
		// Every value, not the first: an HTTP/2 client may split Cookie
		// across several fields, and Get would keep only one of them.
		for _, v := range r.Header.Values(name) {
			forwarded.Add(name, v)
		}
	}

	ctx := context.WithValue(r.Context(), headerKey, forwarded)

	// A batch: one HTTP request for a page's parallel reads rather than one
	// each. The calls run concurrently, and each is answered the moment it
	// finishes - one JSON line, carrying its index - so a component waiting
	// on a fast read paints while a slow sibling's is still running. Every
	// call is answered as it would have been alone: a refusal in the third
	// is that call's answer, not a reason to leave the fourth unanswered.
	if call.Calls != nil {
		if len(call.Calls) == 0 {
			writeReply(w, http.StatusBadRequest, callReply{Error: "a batch needs a non-empty \"calls\" list"})

			return
		}

		// Each call is a goroutine and, often, a database query. The renderer
		// never sends more than MaxBatch; a body that does is not from it.
		if len(call.Calls) > MaxBatch {
			writeReply(w, http.StatusRequestEntityTooLarge, callReply{
				Error: fmt.Sprintf("a batch carries at most %d calls, this one %d", MaxBatch, len(call.Calls)),
			})

			return
		}

		h.serveBatch(ctx, w, call.Calls)

		return
	}

	status, reply := h.dispatch(ctx, call)
	writeReply(w, status, reply)
}

// serveBatch answers a batch as NDJSON, each line as its call completes.
func (h *CallbackHandler) serveBatch(ctx context.Context, w http.ResponseWriter, calls []callRequest) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	// A proxy that buffers would hold the fast answer behind the slow one,
	// which is the one thing this shape exists to avoid.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	for i, one := range calls {
		wg.Add(1)

		go func(index int, one callRequest) {
			defer wg.Done()

			status, reply := h.dispatch(ctx, one)
			line := batchLineFor(index, status, reply)

			mu.Lock()
			defer mu.Unlock()

			_, _ = w.Write(append(line, '\n'))
			if flusher != nil {
				flusher.Flush()
			}
		}(i, one)
	}

	wg.Wait()
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

	result, err := h.call(ctx, call.Function, fn, call.Args)
	if err != nil {
		status, reply := replyFor(err)

		// A refusal is an answer and needs no trace; a failure is a bug,
		// and its whereabouts are the whole of the next step.
		if status == http.StatusInternalServerError && h.Debug {
			reply.Debug = debugFor(call.Function, err)
		}

		return status, reply
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
		// An empty refusal still has to arrive as one. validationErrors is
		// omitted when empty, and a bare 422 reads as a failed call.
		if len(invalid.Errors) == 0 {
			return http.StatusUnprocessableEntity, callReply{ValidationErrors: map[string][]string{"": {"The given data was invalid."}}}
		}

		return http.StatusUnprocessableEntity, callReply{ValidationErrors: invalid.Errors}
	case errors.As(err, &noone):
		return http.StatusUnauthorized, callReply{Unauthenticated: true, Error: noone.Message}
	case errors.As(err, &mayNot):
		return http.StatusForbidden, callReply{Unauthorized: true, Error: mayNot.Message}
	case errors.As(err, &redirect):
		// Nowhere to go is not a redirect. Answered as one it would be a 200
		// with an empty body - a successful null - and a refusal would read as
		// the call succeeding: Redirect(r.URL.Query().Get("next")) with no next.
		if redirect.Location == "" {
			return http.StatusInternalServerError, callReply{Error: "redirect with no location"}
		}

		return http.StatusOK, callReply{Redirect: redirect.Location, RedirectStatus: redirect.Status}
	case errors.As(err, &refused):
		// A refusal is a 4xx or a 5xx. Anything else - Refuse(0, ...) -
		// would reach WriteHeader and panic, losing the reason.
		if refused.Status < 400 || refused.Status > 599 {
			return http.StatusInternalServerError, callReply{Error: fmt.Sprintf("refused with %d, which is not a refusal status: %s", refused.Status, refused.Message)}
		}

		return refused.Status, callReply{Error: refused.Message, RefusalStatus: refused.Status}
	}

	return http.StatusInternalServerError, callReply{Error: err.Error()}
}

// call runs the function, turning a panic into an error.
//
// A panicking host function would otherwise take down the whole server, and
// with it every other render in flight — for what is, from the renderer's
// point of view, one component failing to fetch.
func (h *CallbackHandler) call(ctx context.Context, name string, fn Func, args Args) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			// Logged here, with the stack: the renderer is told only that it
			// panicked, and without this nobody could find out where.
			logf("rsckit: host function %q panicked: %v\n%s", name, r, debug.Stack())
			err = &panicked{value: r, frames: framesHere()}
		}
	}()

	return fn(ctx, args)
}

// MaxBatch is the most calls one batch may carry - the renderer's own limit.
const MaxBatch = 50

// marshal encodes v, turning a failure - an error, or a panic in a result's
// own MarshalJSON - into an error. encoding/json re-panics what it did not
// raise itself, and in a batch that happens on a goroutine net/http never
// started: nothing would recover it, and the whole server would go down.
func marshal(v any) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("encoding the result panicked: %v", r)
		}
	}()

	return json.Marshal(v)
}

// batchLineFor is one batch answer, always a line: a result that cannot be
// encoded is that call's 500, not the end of the batch or the process.
func batchLineFor(index, status int, reply callReply) []byte {
	line, err := marshal(batchLine{Index: index, Status: status, callReply: reply})
	if err != nil {
		logf("rsckit: batch call %d: %v", index, err)
		line, _ = json.Marshal(batchLine{Index: index, Status: http.StatusInternalServerError, callReply: callReply{Error: err.Error()}})
	}

	return line
}

// writeReply encodes before it writes the status. Encoding after meant a
// result that could not be encoded - a NaN, say - went out as a 200 with an
// empty body, and the reason with it went nowhere.
func writeReply(w http.ResponseWriter, status int, reply callReply) {
	body, err := marshal(reply)
	if err != nil {
		logf("rsckit: %v", err)
		status = http.StatusInternalServerError
		body, _ = json.Marshal(callReply{Error: err.Error()})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// Logger receives what the adapter could not answer with: a panic in a host
// function, with its stack, and a result that would not encode. Defaults to
// the standard logger; set it to route these elsewhere.
var Logger = log.Printf

func logf(format string, args ...any) {
	if Logger != nil {
		Logger(format, args...)
	}
}

// panicked is a recovered panic, with the frames it unwound.
type panicked struct {
	value  any
	frames []string
}

func (p *panicked) Error() string { return fmt.Sprintf("host function panicked: %v", p.value) }

// framesHere lists the stack from where a deferred recover runs: the panic
// site and what called it, without the runtime's own frames.
func framesHere() []string {
	pcs := make([]uintptr, 48)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])

	var out []string

	for {
		frame, more := frames.Next()

		if !strings.HasPrefix(frame.Function, "runtime.") {
			out = append(out, fmt.Sprintf("%s:%d %s", frame.File, frame.Line, frame.Function))
		}

		if !more {
			break
		}
	}

	return out
}

// debugFor describes a failure for the renderer: a panic by its frames, an
// error by the chain it wraps - Go errors carry no stack, so the wrapping
// ("loading orders: query: connection refused") is the trace there is.
func debugFor(function string, err error) *debugInfo {
	info := &debugInfo{Type: fmt.Sprintf("%T", err), Message: err.Error()}

	var p *panicked
	if errors.As(err, &p) {
		info.Type = fmt.Sprintf("panic(%T)", p.value)
		info.Trace = p.frames

		return info
	}

	info.Trace = []string{"in host function " + function}

	for e := errors.Unwrap(err); e != nil; e = errors.Unwrap(e) {
		info.Trace = append(info.Trace, fmt.Sprintf("wrapping %T: %v", e, e))
	}

	return info
}
