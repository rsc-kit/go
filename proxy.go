package rsckit

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// The two markers that keep a url neither process owns from bouncing between
// them. The renderer forwards what its route tree does not own back to this
// server with FallbackMarker set; this server forwards what it does not route
// to the renderer with ProxiedMarker set. Each side, seeing the other's mark,
// answers 404 itself instead of asking the same question a second time.
const (
	FallbackMarker = "X-Rsc-Renderer-Fallback"
	ProxiedMarker  = "X-Rsc-Proxied-By-Backend"
)

// Renderer points at the JS process running @rsc-kit/core/host.
type Renderer struct {
	// Target is where it listens — "http://127.0.0.1:5173", or a unix socket
	// via UnixSocket below.
	Target *url.URL

	proxy *httputil.ReverseProxy
}

// NewRenderer proxies to a renderer reachable over TCP.
func NewRenderer(target string) (*Renderer, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("rsckit: renderer target needs an http(s) scheme")
	}

	return newRenderer(u, nil), nil
}

// NewUnixRenderer proxies to a renderer listening on a unix socket.
//
// Worth preferring where both processes are on one machine: there is no port
// to collide with, no loopback interface to accidentally expose, and the
// socket's file permissions are the access control.
func NewUnixRenderer(socketPath string) (*Renderer, error) {
	u, err := url.Parse("http://rsc-renderer")
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}

	return newRenderer(u, &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		// A render holds its connection for the length of the stream, so the
		// pool has to be big enough for concurrent page loads rather than the
		// default's assumption of short requests.
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}), nil
}

func newRenderer(target *url.URL, transport http.RoundTripper) *Renderer {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
			// The renderer needs the real client address for anything that
			// reads it; SetXForwarded also stops a client-supplied
			// X-Forwarded-For from being passed through as if it were ours.
			r.SetXForwarded()
			// Came from here. The renderer answers 404 for what it does not
			// own rather than handing it back to be proxied again.
			r.Out.Header.Set(ProxiedMarker, "1")
		},
		// Belt and braces, and worth knowing which. A streamed render arrives
		// with no Content-Length, and ReverseProxy.flushInterval already
		// forces -1 for those regardless of what is set here — so the shell
		// reaches the browser immediately whether or not this line exists.
		// What it covers is the other case: a response that DOES carry a
		// Content-Length, such as a small prerendered page, where the default
		// would buffer. Cheap, and it removes a difference nobody would think
		// to look for.
		FlushInterval: -1,
		Transport:     transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// A renderer that is down is a 502, not a panic. Say which side
			// failed: the alternative is a bare 502 that reads as the app's.
			http.Error(w, "rsc renderer unreachable: "+err.Error(), http.StatusBadGateway)
		},
	}

	return &Renderer{Target: target, proxy: proxy}
}

func (r *Renderer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// The renderer already asked its own route table and the answer was no;
	// it is asking whether this server routes the url. Reaching the proxy
	// means nothing here did either.
	if req.Header.Get(FallbackMarker) != "" {
		http.NotFound(w, req)

		return
	}

	r.proxy.ServeHTTP(w, req)
}

// Handler serves an rsc-kit app: the renderer for pages, and the callback
// endpoint for what the renderer asks back.
type Handler struct {
	Renderer *Renderer
	Callback *CallbackHandler

	// CallbackPath is where the renderer POSTs host calls. It must match the
	// endpoint httpHostCalls was given.
	//
	// Serving it from the same mux as the app is the convenient arrangement
	// and the one to be careful with: it is reachable by anyone who can reach
	// the app, so the shared secret is the only thing in front of it. Mounting
	// it on a separate listener bound to loopback is the safer shape, and
	// Callback can be used on its own for that.
	CallbackPath string
}

// NewHandler wires a renderer and a callback endpoint together.
func NewHandler(renderer *Renderer, callback *CallbackHandler, callbackPath string) *Handler {
	if callbackPath == "" {
		callbackPath = "/__rsc/host-call"
	}

	return &Handler{Renderer: renderer, Callback: callback, CallbackPath: callbackPath}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == h.CallbackPath {
		h.Callback.ServeHTTP(w, r)

		return
	}

	h.Renderer.ServeHTTP(w, r)
}
