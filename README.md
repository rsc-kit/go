# rsc-kit for Go

Host an rsc-kit application from a Go server.

```sh
go get github.com/rsc-kit/go
```

Go owns the request — sessions, auth, the database. The renderer owns
rendering, because that half is React and there is no way around it. A server
component reaches Go by calling `rpc()`, which arrives here as an ordinary
POST; a `middleware.ts` names guards Go runs before a page renders; a server
action the browser calls is a Go function the build wrote a stub for.

```go
reg := rsckit.NewRegistry()

// A plain Go function. Its parameter and result types are written to
// rsc-host.json, so the app's rpc('Orders.recent', 5) is typed as []Order.
reg.Handle("Orders.recent", func(ctx context.Context, limit int) ([]Order, error) {
    // The visitor's own cookie, forwarded from the page request — so this
    // query runs as them, not as nobody.
    session := rsckit.HeadersFrom(ctx).Get("Cookie")

    return db.RecentOrders(ctx, session, limit)
})

reg.Middleware("auth", func(ctx context.Context, _ string) error {
    if !signedIn(rsckit.HeadersFrom(ctx)) {
        return rsckit.Redirect("/login")
    }
    return nil
})

callback, err := rsckit.NewCallbackHandler(reg, os.Getenv("RSC_HOST_CALL_SECRET"))
http.Handle("POST /__rsc/host-call", callback)

// The package never builds a server, so its timeouts are yours. Without
// them a client that sends its headers a byte at a time holds a goroutine
// for as long as it likes.
server := &http.Server{
    Addr:              "127.0.0.1:8080",
    ReadHeaderTimeout: 5 * time.Second,
    IdleTimeout:       120 * time.Second,
}
log.Fatal(server.ListenAndServe())
```

The JavaScript side is what `bun create rsc-kit` writes, plus two lines in
`.env`:

```ini
RSC_BACKEND=http://127.0.0.1:8080
RSC_HOST_CALL_SECRET=a-long-random-string
```

The renderer reads them in development (`vite`) and in production (the built
server) and wires `rpc()` to the endpoint. There is no JavaScript to write for
Go; [`examples/go-backend`](https://github.com/rsc-kit/rsc-kit/tree/main/examples/go-backend) in the engine's repository is the whole arrangement, runnable.

## Why there is no frame protocol here

Reimplementing a host used to mean a binary framing over two unix sockets.
That was never the expensive part — the expensive part is everything around
it: partial-navigation depth arithmetic, redirect delivery, cookie forwarding,
prerendered variants, PPR. `@rsc-kit/core` already implements all of that, so
this adapter does not. What a backend implements is one endpoint, and the
contract is written down at
[rsc-kit.dev/hosts/your-own-backend](https://rsc-kit.dev/hosts/your-own-backend).

What it costs: a JS process alongside the Go binary. If you want one static
artifact, that is only reachable when every route is prerendered — then the
renderer is a build-time dependency and Go serves files.

## Two things to get right

**The callback endpoint is not public.** It runs functions by name, with none
of the app's routing or authorization in front of it. `NewCallbackHandler`
refuses to be built without a shared secret, and checks it in constant time.
Restrict the path at the web server as well, or mount it on a separate
listener bound to loopback.

**Register at startup, once.** `Register` and `Middleware` panic on a
duplicate name rather than overwriting. A silent overwrite survives a refactor
and then answers the wrong query.

## What a function can answer

Return a value and it is the result. Return one of these and the render is
told what happened, rather than handed a 500:

| return | the render gets |
| --- | --- |
| `rsckit.Invalid(map)` / `rsckit.InvalidField("name", "…")` | 422, each message under its input on the form that submitted |
| `rsckit.Unauthenticated()` | 401, the engine's own authentication error |
| `rsckit.Unauthorized("…")` | 403 |
| `rsckit.Redirect("/login")` | the browser goes there — sent as a 200 with the destination in the body, because an HTTP client would follow a real 3xx |
| `rsckit.Refuse(429, "slow down")` | that status, kept — a throttle's 429 is not a broken server |
| any other `error` | 500, with the message |

Wrapped errors still answer as what they are: `errors.As` finds the refusal
inside `fmt.Errorf("…: %w", err)`.

## Route middleware

```ts
// app/admin/middleware.ts
export const middleware = ['auth', 'can:manage-orders', 'throttle:60,1']
```

The renderer sends that list before anything at or below the directory
renders — including a page it froze at build time, before the file is served.
`Middleware(name, guard)` answers each name; the guard receives what follows
the colon (`"manage-orders"`, `"60,1"`, `""`). Guards run in order and stop at
the first refusal. A name nothing is registered for is a refusal, not a pass:
a declared check that silently does not happen is the failure this exists to
prevent.

## Typed functions

`Handle` and `HandleAction` take an ordinary Go function: any number of
parameters of any type JSON can carry, bound from `rpc()`'s arguments in
order, with an optional leading `context.Context`. A trailing pointer may be
left out by the caller; a variadic parameter takes the rest. It returns a
value and an error, only an error, or only a value.

The parameter and result types go into `rsc-host.json` as JSON Schema, and
the build turns them into TypeScript: each struct an interface named for its
Go type, by its `json` tags. `Register` and `RegisterAction` keep the untyped
`func(ctx, rsckit.Args) (any, error)` form.

## Server actions

```go
reg.HandleAction("ordersCancel", "Orders.cancel", func(ctx context.Context, id int) error { … })
reg.WriteManifest("rsc-host.json")
```

A form can post to an action directly; its fields arrive as the first
parameter, decoded into the struct.

`rsc-host.json` lists the actions and every registered function. The build
writes `server-actions.generated.ts` beside the app, exporting `ordersCancel`;
a client component imports and calls it, and the call arrives here as
`Orders.cancel`. The function names become the type of `rpc()`'s first
argument, so `rpc('Orders.recnet')` fails the typecheck.

Have the build write it, so it cannot go stale. Give your binary a flag that
writes the manifest and exits, and name it in the Vite config:

```ts
rscKit({ hostManifest: { command: ['go', 'run', './backend', '-manifest', '../rsc-host.json'] } })
```

It runs as `vite` and `vite build` start. A build fails if it fails; dev
reports it and uses the file already there.

`rsckit.Revalidate(ctx, "orders")` inside an action marks a region stale, so
the answer carries it re-rendered instead of the browser being told to ask
again.

## Batches

Calls the renderer issued in one tick of a render — sibling components each
awaiting `rpc()` — arrive as one POST. They run **concurrently**, and each is
answered on its own NDJSON line the moment it finishes, with the status it
would have had alone, so a fast read paints while a slow one is still going.
`CallbackHandler` does this; each call keeps its own `Revalidate` and gets
its own copy of the forwarded headers.

Concurrently is the difference from Laravel, which runs a batch one call at a
time: a function that touches shared state must be safe to run beside itself.
A batch carries at most `rsckit.MaxBatch` (50) calls, the renderer's own
limit; a larger one is refused with 413.

## What a function sees

- `args.Bind(&a, &b)` decodes positional arguments. Too few is an error;
  extra ones are ignored.
- `rsckit.HeadersFrom(ctx)` has the forwarded `Cookie` and `Authorization`.
  Empty during a build-time render, which has no visitor.
- A panic becomes an error for that one call. It does not take down the
  server, and every other render in flight survives it — including a panic
  in a result's own `MarshalJSON`. The panic is logged with its stack
  through `rsckit.Logger` (the standard logger unless you set it).

## Go in front

The renderer can face the internet and forward what it does not own to
`RSC_BACKEND` — a Go route, a webhook, an upload. Or Go faces it:
`NewRenderer(url)` is a streaming reverse proxy, `NewHandler` routes the
callback path to the endpoint and everything else to it, and both honour the
markers that keep a url neither side owns from bouncing between them.

## Tests

`go test ./...` covers the contract from this side. The end-to-end proof — a
real page rendered by the engine with data, guards, actions and batches from a
Go server — lives with the engine, in
[`packages/core/tests/js/goAdapter*.test.ts`](https://github.com/rsc-kit/rsc-kit/tree/main/packages/core/tests/js),
which build a host server on this module, run it and render against it. A
contract change is released here first, and the engine's fixture follows.

Guides, the contract every backend answers, and a runnable example at
[rsc-kit.dev/hosts/go](https://rsc-kit.dev/hosts/go).
