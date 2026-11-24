package rsckit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	rsckit "github.com/rsc-kit/go"
)

type appKey struct{}

type requestIDKey struct{}

// ask sends the calls named, in order, and returns each answer by name.
func ask(t *testing.T, reg *rsckit.Registry, options []rsckit.BuildOption, names ...string) map[string]map[string]any {
	t.Helper()

	var in, out bytes.Buffer
	for i, name := range names {
		fmt.Fprintf(&in, `{"id":%d,"function":%q,"args":[]}`+"\n", i, name)
	}

	if err := reg.ServeCalls(&in, &out, options...); err != nil {
		t.Fatal(err)
	}

	answers := map[string]map[string]any{}
	for _, line := range strings.Split(out.String(), "\n") {
		var answer map[string]any
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &answer); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		answers[names[int(answer["id"].(float64))]] = answer
	}

	return answers
}

func TestServeCallsAnswersWhatIsTheSameForEveryone(t *testing.T) {
	reg := rsckit.NewRegistry()
	reg.Handle("plain", func() []string { return nil })
	reg.Handle("falsy", func() bool { return false })
	// What a database driver does with a context: deadlines, cancellation, goroutines.
	reg.Handle("query", func(ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		ctx, stop := context.WithCancelCause(context.WithoutCancel(ctx))
		defer stop(nil)
		slog.DebugContext(ctx, "query")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:1/", nil)
		if err != nil {
			return "", err
		}
		_, _ = http.DefaultClient.Do(req)

		return "rows", ctx.Err()
	})
	reg.Handle("changed", func(ctx context.Context) error {
		rsckit.Revalidate(ctx, "a")
		_, _ = rsckit.Checking(ctx)

		return reg.Changed(ctx, "a")
	})
	reg.Handle("fails", func() error { return errors.New("no such table") })
	reg.Handle("panics", func() { panic("oh") })
	reg.Handle("unencodable", func() any { return map[string]any{"f": func() {}} })

	answers := ask(t, reg, nil, "plain", "falsy", "query", "changed", "fails", "panics", "unencodable", "nobody")

	for name, status := range map[string]float64{
		"plain": 200, "falsy": 200, "query": 200, "changed": 200, "fails": 500, "panics": 500, "unencodable": 500, "nobody": 404,
	} {
		if got := answers[name]; got["status"] != status || got["visitor"] != nil {
			t.Errorf("%s answered %v", name, got)
		}
	}

	if answers["falsy"]["result"] != false || answers["query"]["result"] != "rows" {
		t.Errorf("results %v %v", answers["falsy"], answers["query"])
	}
	if answers["fails"]["error"] != "no such table" || answers["fails"]["debug"] == nil {
		t.Errorf("a failure says where: %v", answers["fails"])
	}
}

func TestServeCallsSaysWhatReadTheVisitor(t *testing.T) {
	reg := rsckit.NewRegistry()
	reg.Handle("header", func(ctx context.Context) string { return rsckit.HeadersFrom(ctx).Get("Cookie") })
	reg.Handle("ip", func(ctx context.Context) string { return rsckit.ClientIP(ctx) })
	reg.Handle("agent", func(ctx context.Context) string { return rsckit.ClientUserAgent(ctx) })
	reg.Handle("cookie", func(ctx context.Context) { rsckit.SetCookie(ctx, &http.Cookie{Name: "a"}) })
	reg.Handle("flash", func(ctx context.Context) { rsckit.Flash(ctx, rsckit.FlashSuccess, "Saved.") })
	// What an app's own session middleware would have put there.
	reg.Handle("user", func(ctx context.Context) any { return ctx.Value(appKey{}) })
	reg.Handle("derived", func(ctx context.Context) any {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		return ctx.Value(&appKey{})
	})
	reg.Handle("string", func(ctx context.Context) any { return ctx.Value("user") })
	reg.Handle("thenFails", func(ctx context.Context) (any, error) {
		return nil, fmt.Errorf("no user: %v", ctx.Value(appKey{}))
	})

	names := []string{"header", "ip", "agent", "cookie", "flash", "user", "derived", "string", "thenFails"}
	answers := ask(t, reg, nil, names...)

	for _, name := range names {
		if answers[name]["visitor"] == nil || answers[name]["result"] != nil {
			t.Errorf("%s answered %v", name, answers[name])
		}
	}

	if got := fmt.Sprint(answers["string"]["visitor"]); got != "[the context value user]" {
		t.Errorf("said %s", got)
	}
	if got := fmt.Sprint(answers["user"]["visitor"]); !strings.Contains(got, "appKey") {
		t.Errorf("said %s", got)
	}
}

func TestServeCallsNotTheVisitor(t *testing.T) {
	reg := rsckit.NewRegistry()
	reg.Handle("logged", func(ctx context.Context) any { return ctx.Value(requestIDKey{}) })

	named := []rsckit.BuildOption{rsckit.NotTheVisitor("github.com/rsc-kit/go_test.requestIDKey")}

	if got := ask(t, reg, named, "logged")["logged"]; got["visitor"] != nil {
		t.Errorf("answered %v", got)
	}
	if got := ask(t, reg, nil, "logged")["logged"]; got["visitor"] == nil {
		t.Errorf("answered %v", got)
	}
}

func TestServeCallsOutlivesALineThatIsNotACall(t *testing.T) {
	reg := rsckit.NewRegistry()
	reg.Handle("echo", func(v any) any { return v })

	var out bytes.Buffer
	in := strings.NewReader("not json\n\n{\"id\":1}\n{\"id\":2,\"function\":\"echo\",\"args\":\"x\"}\n{\"id\":3,\"function\":\"echo\",\"args\":[7]}")

	quiet := rsckit.Logger
	rsckit.Logger = nil
	defer func() { rsckit.Logger = quiet }()

	if err := reg.ServeCalls(in, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":3,"status":200,"result":7`) || strings.Count(out.String(), `"status":400`) != 2 {
		t.Errorf("answered %s", out.String())
	}
}

func TestTheManifestSaysHowABuildAsks(t *testing.T) {
	reg := rsckit.NewRegistry()
	if reg.Manifest().Calls != nil {
		t.Error("an app that said nothing answers no build")
	}

	reg.BuildCalls("-calls")
	if got := reg.Manifest().Calls; len(got) != 1 || got[0] != "-calls" {
		t.Errorf("calls %v", got)
	}
}
