package rsckit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
)

// BuildCalls says how the command that writes the manifest is asked to answer
// a build's calls instead: the arguments added to it. The build then asks this
// backend for what rpc() reads while it renders pages, and stores the answer
// with the page - unless the function read who is asking:
//
//	reg.BuildCalls("-calls")
//
//	if *calls {
//		if err := reg.ServeCalls(os.Stdin, os.Stdout); err != nil {
//			log.Fatal(err)
//		}
//		return
//	}
func (r *Registry) BuildCalls(args ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buildCalls = append([]string(nil), args...)
}

// NotTheVisitor names context keys a function may read without its answer
// being one visitor's, for ServeCalls: a package, or one key type in it.
//
//	reg.ServeCalls(os.Stdin, os.Stdout, rsckit.NotTheVisitor("github.com/acme/tracing", "main.requestIDKey"))
//
// A package covers those beneath it. The standard library's own keys, and
// those of the usual tracing and database packages, are already known.
func NotTheVisitor(keys ...string) BuildOption {
	return func(b *buildServer) { b.ignored = append(b.ignored, keys...) }
}

// BuildOption changes how ServeCalls answers.
type BuildOption func(*buildServer)

// ServeCalls answers a build's calls, one JSON object a line each way, until
// in ends.
//
// A build has no visitor, so a function that reads one has no answer to store.
// What it reads, it reads through its context, and that is what is watched:
// the request's headers, its address, its browser, a cookie set, and any value
// the app's own middleware would have put there - a signed-in user. Such a
// call is answered as needing a visitor, and the page streams it per request,
// as it would have without this. A function that takes no context, or only
// hands it to a database, is answered once, for everyone.
//
// Package state is not watched: a function that reads the visitor from
// anywhere but its context should say `await connection()` on the page.
func (r *Registry) ServeCalls(in io.Reader, out io.Writer, options ...BuildOption) error {
	b := &buildServer{
		// Debug: what fails here fails a build, and the build is where
		// someone reads why.
		handler: &CallbackHandler{registry: r, Debug: true},
		out:     out,
		ignored: append([]string(nil), notTheVisitor...),
		slots:   make(chan struct{}, MaxBatch),
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		b.module = info.Main.Path
	}

	for _, option := range options {
		option(b)
	}

	reader := bufio.NewReader(in)

	for {
		line, err := reader.ReadBytes('\n')

		if len(strings.TrimSpace(string(line))) > 0 {
			b.take(line)
		}

		if err != nil {
			b.running.Wait()

			if !errors.Is(err, io.EOF) {
				return err
			}

			return b.failed()
		}
	}
}

// notTheVisitor are packages whose context keys carry a trace, a logger or a
// transaction: read by nearly every query an instrumented app makes, and the
// same answer whoever asked.
var notTheVisitor = []string{
	"cloud.google.com/go",
	"github.com/DataDog",
	"github.com/aws",
	"github.com/getsentry/sentry-go",
	"github.com/go-logr/logr",
	"github.com/jackc",
	"github.com/redis",
	"github.com/rs/zerolog",
	"go.mongodb.org",
	"go.opentelemetry.io",
	"go.uber.org",
	"golang.org/x",
	"google.golang.org",
	"gorm.io",
}

type buildServer struct {
	handler *CallbackHandler
	ignored []string
	module  string
	slots   chan struct{}
	running sync.WaitGroup

	mu       sync.Mutex
	out      io.Writer
	writeErr error
}

type buildCall struct {
	ID       json.RawMessage   `json:"id"`
	Function string            `json:"function"`
	Args     []json.RawMessage `json:"args"`
}

type buildAnswer struct {
	ID     json.RawMessage `json:"id"`
	Status int             `json:"status"`
	callReply
}

type buildVisitor struct {
	ID      json.RawMessage `json:"id"`
	Visitor []string        `json:"visitor"`
}

// take answers one line. A line that is not a call is not worth the process:
// the calls after it are still waiting.
func (b *buildServer) take(line []byte) {
	var call buildCall
	err := json.Unmarshal(line, &call)

	// An id survives arguments that do not decode - enough to answer it.
	if len(call.ID) == 0 {
		logf("rsckit: a line that is not a build's call: %s", strings.TrimSpace(string(line)))

		return
	}

	if err != nil || call.Function == "" {
		b.answer(buildAnswer{ID: call.ID, Status: http.StatusBadRequest, callReply: callReply{Error: "malformed build call"}})

		return
	}

	b.running.Add(1)
	b.slots <- struct{}{}

	go func() {
		defer func() {
			<-b.slots
			b.running.Done()
		}()

		b.run(call)
	}()
}

func (b *buildServer) run(call buildCall) {
	seen := &visitorSeen{reads: map[string]bool{}}
	ctx := &buildContext{Context: context.Background(), server: b, seen: seen}

	// Nothing is purged and no cookie goes anywhere; the keys are there so
	// that saying so costs a function nothing.
	pending := &pendingPurge{}
	ctx.Context = context.WithValue(ctx.Context, purgeKey, pending)
	ctx.Context = context.WithValue(ctx.Context, cookieKey, &cookieBox{})

	status, reply := b.handler.dispatch(ctx, callRequest{Function: call.Function, Args: call.Args}, false)

	if reads := seen.all(); len(reads) > 0 {
		b.answer(buildVisitor{ID: call.ID, Visitor: reads})

		return
	}

	b.answer(buildAnswer{ID: call.ID, Status: status, callReply: reply})
}

// answer writes one line, on a line of its own whatever the app printed.
func (b *buildServer) answer(answer any) {
	line, err := marshal(answer)
	if err != nil {
		var id json.RawMessage

		if a, ok := answer.(buildAnswer); ok {
			id = a.ID
		}

		line, _ = json.Marshal(buildAnswer{ID: id, Status: http.StatusInternalServerError, callReply: callReply{Error: err.Error()}})
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, err := b.out.Write(append(append([]byte{'\n'}, line...), '\n')); err != nil && b.writeErr == nil {
		b.writeErr = err
	}
}

func (b *buildServer) failed() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.writeErr
}

type visitorSeen struct {
	mu    sync.Mutex
	reads map[string]bool
}

func (s *visitorSeen) note(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads[what] = true
}

func (s *visitorSeen) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	reads := make([]string, 0, len(s.reads))
	for what := range s.reads {
		reads = append(reads, what)
	}
	sort.Strings(reads)

	return reads
}

// buildContext is the context a call gets during a build: no visitor, and a
// note of every value a function looked for that a visitor would have had.
type buildContext struct {
	context.Context
	server *buildServer
	seen   *visitorSeen
}

func (c *buildContext) Value(key any) any {
	if what := c.server.visitors(key); what != "" {
		c.seen.note(what)
	}

	return c.Context.Value(key)
}

// visitors is what of the visitor's a context key carries, or nothing.
func (b *buildServer) visitors(key any) string {
	if own, ok := key.(ctxKey); ok {
		switch own {
		case headerKey:
			return "a request header"
		case clientIPKey:
			return "the visitor's address"
		case clientUserAgentKey:
			return "the visitor's browser"
		case cookieKey:
			return "a cookie"
		case pageKey:
			return "the page's url"
		}

		return ""
	}

	// context.WithCancel and the rest ask their parent whether it is a
	// cancelling one of theirs, with a key of this type and no name.
	if key == nil || key == cancelKey {
		return ""
	}

	t := reflect.TypeOf(key)
	for t.Kind() == reflect.Pointer && t.Name() == "" {
		t = t.Elem()
	}

	pkg := t.PkgPath()
	name := t.String()

	if pkg != "" {
		name = pkg + "." + t.Name()
	}

	// This package's own other keys, and the standard library's: a server, a
	// trace, a transaction. http.LocalAddrContextKey is the server's address.
	// Not "no dot in its path": main has none, nor has a module named backend.
	if pkg == "github.com/rsc-kit/go" || (b.standard(pkg) && !covers(b.module, pkg)) {
		return ""
	}

	for _, ignored := range b.ignored {
		if name == ignored || covers(ignored, pkg) {
			return ""
		}
	}

	if pkg == "" {
		return fmt.Sprintf("the context value %v", key)
	}

	return "the context value " + name
}

// standard is whether a package is the standard library's, by its first path
// element: the ones that put keys in a context or look for them.
func (b *buildServer) standard(pkg string) bool {
	first, _, _ := strings.Cut(pkg, "/")

	switch first {
	case "context", "crypto", "database", "encoding", "go", "html", "internal", "io", "iter", "log", "mime", "net",
		"os", "plugin", "reflect", "runtime", "sync", "syscall", "testing", "text", "time", "vendor":
		return true
	}

	return false
}

func covers(prefix, pkg string) bool {
	return prefix != "" && (pkg == prefix || strings.HasPrefix(pkg, prefix+"/"))
}

// cancelKey is the context package's own key, as a context sees it asked for.
var cancelKey = func() any {
	probe := &probeContext{Context: context.Background(), done: make(chan struct{})}
	_, cancel := context.WithCancel(probe)
	cancel()

	return probe.asked
}()

type probeContext struct {
	context.Context
	done  chan struct{}
	asked any
}

func (p *probeContext) Done() <-chan struct{} { return p.done }

func (p *probeContext) Value(key any) any {
	if p.asked == nil {
		p.asked = key
	}

	return nil
}
