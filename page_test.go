package rsckit

import (
	"context"
	"testing"
)

// The call comes from the renderer, so r.RemoteAddr is the same for every
// visitor: a rate limit keyed on it was one bucket for everyone.
func TestClientIPIsTheOneTheRendererSent(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("Who.ip", func(ctx context.Context, _ Args) (any, error) { return ClientIP(ctx), nil })
	})

	reply := decode(t, post(h, `{"function":"Who.ip","args":[]}`, map[string]string{ClientIPHeader: "203.0.113.7"}))
	if reply.Result != "203.0.113.7" {
		t.Fatalf("ClientIP = %v", reply.Result)
	}

	reply = decode(t, post(h, `{"function":"Who.ip","args":[]}`, nil))
	if reply.Result != "" {
		t.Fatalf("with none sent, ClientIP = %v", reply.Result)
	}
}

// Without the secret nothing runs, so the header cannot be believed from a
// stranger: the endpoint refuses before any function sees it.
func TestClientIPIsNeverReadWithoutTheSecret(t *testing.T) {
	ran := false
	h := handler(t, func(r *Registry) {
		r.Register("Who.ip", func(ctx context.Context, _ Args) (any, error) { ran = true; return ClientIP(ctx), nil })
	})

	rec := post(h, `{"function":"Who.ip","args":[]}`, map[string]string{SecretHeader: "wrong", ClientIPHeader: "1.2.3.4"})
	if rec.Code < 400 || ran {
		t.Fatalf("status %d, ran %v", rec.Code, ran)
	}
}

// The call's own User-Agent is the renderer's runtime: a log of who made a
// request saw the same one for every visitor.
func TestClientUserAgentIsTheOneTheRendererSent(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Register("Who.agent", func(ctx context.Context, _ Args) (any, error) { return ClientUserAgent(ctx), nil })
	})

	reply := decode(t, post(h, `{"function":"Who.agent","args":[]}`, map[string]string{ClientUserAgentHeader: "Mozilla/5.0 (Visitor)"}))
	if reply.Result != "Mozilla/5.0 (Visitor)" {
		t.Fatalf("ClientUserAgent = %v", reply.Result)
	}

	reply = decode(t, post(h, `{"function":"Who.agent","args":[]}`, nil))
	if reply.Result != "" {
		t.Fatalf("with none sent, ClientUserAgent = %v", reply.Result)
	}
}

func TestAGuardReadsThePageItIsAskedAbout(t *testing.T) {
	var seen Page
	var had bool

	h := handler(t, func(r *Registry) {
		r.Middleware("owns-order", func(ctx context.Context, _ string) error {
			seen, had = PageFrom(ctx)

			return nil
		})
	})

	reply := decode(t, post(h, `{"function":"__rsc.middleware","args":[["owns-order"],{"url":"https://app.test/orders/7?tab=items","params":{"order":"7"}}]}`, nil))
	if reply.Result != true || !had || seen.URL != "https://app.test/orders/7?tab=items" || seen.Params["order"] != "7" {
		t.Fatalf("result %v, page %+v (%v)", reply.Result, seen, had)
	}

	// An engine that sent no page: the guard is told so, rather than handed an empty one.
	decode(t, post(h, `{"function":"__rsc.middleware","args":[["owns-order"]]}`, nil))
	if had {
		t.Fatalf("PageFrom reported a page nobody sent: %+v", seen)
	}
}
