package rsckit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every refusal has its own field and its own status, matching what Laravel
// answers and what the renderer reads - unauthenticated becomes the engine's
// 401, a redirect travels as a 200 with the destination in the body, a
// throttle keeps its 429. The renderer never parses a message to tell them
// apart, so the fields are the contract.
func TestEachRefusalTravelsAsItsOwnField(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("noone", func(context.Context, Args) (any, error) { return nil, Unauthenticated() })
		r.Register("mayNot", func(context.Context, Args) (any, error) { return nil, Unauthorized("not yours") })
		r.Register("elsewhere", func(context.Context, Args) (any, error) { return nil, Redirect("/login") })
		r.Register("elsewhere303", func(context.Context, Args) (any, error) { return nil, Redirect("/done", 303) })
		r.Register("slowDown", func(context.Context, Args) (any, error) { return nil, Refuse(429, "") })
	})

	cases := []struct {
		fn     string
		status int
		check  func(t *testing.T, reply callReply)
	}{
		{"noone", 401, func(t *testing.T, reply callReply) {
			if !reply.Unauthenticated || reply.Error != "Unauthenticated." {
				t.Fatalf("reply = %+v", reply)
			}
		}},
		{"mayNot", 403, func(t *testing.T, reply callReply) {
			if !reply.Unauthorized || reply.Error != "not yours" {
				t.Fatalf("reply = %+v", reply)
			}
		}},
		{"elsewhere", 200, func(t *testing.T, reply callReply) {
			if reply.Redirect != "/login" || reply.RedirectStatus != 0 || reply.Error != "" {
				t.Fatalf("reply = %+v", reply)
			}
		}},
		{"elsewhere303", 200, func(t *testing.T, reply callReply) {
			if reply.Redirect != "/done" || reply.RedirectStatus != 303 {
				t.Fatalf("reply = %+v", reply)
			}
		}},
		{"slowDown", 429, func(t *testing.T, reply callReply) {
			if reply.RefusalStatus != 429 || reply.Error != "Too Many Requests" {
				t.Fatalf("reply = %+v", reply)
			}
		}},
	}

	for _, c := range cases {
		t.Run(c.fn, func(t *testing.T) {
			rec := post(h, `{"function":"`+c.fn+`","args":[]}`, nil)
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, c.status, rec.Body.String())
			}

			c.check(t, decode(t, rec))
		})
	}
}

// Anything wrapped still answers as what it is: a guard wrapping a refusal
// with context should not turn a 401 into a 500.
func TestAWrappedRefusalIsStillARefusal(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("wrapped", func(context.Context, Args) (any, error) {
			return nil, wrap("checking session", Unauthenticated())
		})
	})

	rec := post(h, `{"function":"wrapped","args":[]}`, nil)
	if rec.Code != 401 || !decode(t, rec).Unauthenticated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }
func wrap(msg string, err error) error {
	return &wrapped{msg: msg, err: err}
}

// The reserved call. A route.ts names guards; the renderer sends the list and
// reads a literal true or refuses the render.
func TestRouteMiddlewareRunsEveryGuardInOrderAndAnswersTrue(t *testing.T) {
	var ran []string

	h := handler(t, func(r *Registry) {
		r.Middleware("auth", func(_ context.Context, param string) error {
			ran = append(ran, "auth:"+param)

			return nil
		})
		r.Middleware("can", func(_ context.Context, param string) error {
			ran = append(ran, "can:"+param)

			return nil
		})
		r.Middleware("throttle", func(_ context.Context, param string) error {
			ran = append(ran, "throttle:"+param)

			return nil
		})
	})

	rec := post(h, `{"function":"__rsc.middleware","args":[["auth","can:view,admin","throttle:60,1"]]}`, nil)
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}

	if got, _ := decode(t, rec).Result.(bool); !got {
		t.Fatalf("result = %s, want true", rec.Body.String())
	}

	// The parameter is everything after the first colon, commas included: a
	// parser splitting on commas turns throttle:60,1 into a throttle of 60 and
	// a guard named 1.
	if strings.Join(ran, " ") != "auth: can:view,admin throttle:60,1" {
		t.Fatalf("ran = %v", ran)
	}
}

func TestRouteMiddlewareStopsAtTheFirstRefusalWithItsKind(t *testing.T) {
	inner := false

	h := handler(t, func(r *Registry) {
		r.Middleware("auth", func(ctx context.Context, _ string) error {
			if HeadersFrom(ctx).Get("Cookie") == "" {
				return Redirect("/login")
			}

			return nil
		})
		r.Middleware("can", func(context.Context, string) error {
			inner = true

			return nil
		})
	})

	rec := post(h, `{"function":"__rsc.middleware","args":[["auth","can:edit"]]}`, nil)
	reply := decode(t, rec)

	if rec.Code != 200 || reply.Redirect != "/login" {
		t.Fatalf("status = %d, reply = %+v", rec.Code, reply)
	}

	if inner {
		t.Fatal("an inner guard ran after the outer one refused")
	}

	// With the session it is a pass, and the inner guard is asked.
	rec = post(h, `{"function":"__rsc.middleware","args":[["auth","can:edit"]]}`, map[string]string{"Cookie": "session=1"})
	if got, _ := decode(t, rec).Result.(bool); !got || !inner {
		t.Fatalf("with a session: %d %s, inner=%v", rec.Code, rec.Body.String(), inner)
	}
}

// A guard the route names and this host does not have is not "no guard". It
// is a check that silently does not happen, and the only safe reading is no.
func TestAnUnknownGuardRefusesRatherThanPassing(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Middleware("auth", func(context.Context, string) error { return nil })
	})

	rec := post(h, `{"function":"__rsc.middleware","args":[["auth","verified"]]}`, nil)
	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
	}

	msg := decode(t, rec).Error
	if !strings.Contains(msg, `"verified"`) || !strings.Contains(msg, "[auth]") {
		t.Fatalf("error = %q, want the missing name and what exists", msg)
	}
}

func TestTheReservedNameCannotBeRegisteredAsAFunction(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering __rsc.middleware did not panic")
		}
	}()

	NewRegistry().Register(MiddlewareFunction, func(context.Context, Args) (any, error) { return true, nil })
}

// The build reads rsc-host-actions.json and writes a "use server" module from
// it; this is the Go side of that handoff.
func TestTheActionManifestIsWhatTheBuildReads(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterAction("ordersCancel", "Orders.cancel", func(context.Context, Args) (any, error) { return nil, nil })
	reg.RegisterAction("profileUpdate", "Profile.update", func(context.Context, Args) (any, error) { return nil, nil })
	reg.Register("Orders.recent", func(context.Context, Args) (any, error) { return nil, nil })

	path := filepath.Join(t.TempDir(), "rsc-host-actions.json")
	if err := reg.WriteActionManifest(path); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, _ := os.ReadFile(path)

	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %s", raw)
	}

	// Actions only. A read a server component calls is not something the
	// browser should be handed a stub for.
	want := map[string]string{"ordersCancel": "Orders.cancel", "profileUpdate": "Profile.update"}
	if len(manifest) != len(want) || manifest["ordersCancel"] != want["ordersCancel"] || manifest["profileUpdate"] != want["profileUpdate"] {
		t.Fatalf("manifest = %v, want %v", manifest, want)
	}

	// And the action is callable under its registered name.
	if _, ok := reg.lookup("Orders.cancel"); !ok {
		t.Fatal("the action was not registered as a function")
	}
}

// With Go in front, the renderer forwards what its route tree does not own
// back here. If nothing here routes it either, that must be a 404 - not
// another trip to the renderer, which would ask the same question forever.
func TestAFallbackFromTheRendererIsNotProxiedBackToIt(t *testing.T) {
	trips := 0
	renderer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trips++

		// What the renderer expects to see on a request this server forwarded.
		if r.Header.Get(ProxiedMarker) != "1" {
			t.Errorf("proxied request lacks %s", ProxiedMarker)
		}

		_, _ = io.WriteString(w, "page")
	}))
	defer renderer.Close()

	r, _ := NewRenderer(renderer.URL)
	front := httptest.NewServer(r)
	defer front.Close()

	// An ordinary page goes through, marked.
	res, _ := http.Get(front.URL + "/docs")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if string(body) != "page" || trips != 1 {
		t.Fatalf("page: %q after %d trips", body, trips)
	}

	// The renderer handing back a url it does not own stops here.
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/nothing-owns-this", nil)
	req.Header.Set(FallbackMarker, "1")

	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()

	if res.StatusCode != 404 || trips != 1 {
		t.Fatalf("fallback: status %d after %d trips, want 404 and no new trip", res.StatusCode, trips)
	}
}
