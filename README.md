# rsc-kit for Go

Host an rsc-kit application from a Go server.

Go owns the request — sessions, auth, the database. The renderer owns
rendering, because that half is React and there is no way around it. A server
component reaches Go by calling `rpc()`, which arrives here as an ordinary
POST; a `middleware.ts` names guards Go runs before a page renders; a server
action the browser calls is a Go function the build wrote a stub for.

```go
reg := rsckit.NewRegistry()

reg.Register("Orders.recent", func(ctx context.Context, args rsckit.Args) (any, error) {
    var limit int
    if err := args.Bind(&limit); err != nil {
        return nil, err
    }

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
```

The JavaScript side is what `bun create rsc-kit` writes, plus two lines in
`.env`:

```ini
RSC_BACKEND=http://127.0.0.1:8080
RSC_HOST_CALL_SECRET=a-long-random-string
```

The renderer reads them in development (`vite`) and in production (the built
server) and wires `rpc()` to the endpoint. There is no JavaScript to write for
Go; `examples/go-backend` in the repository is the whole arrangement, runnable.

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

## Server actions

```go
reg.RegisterAction("ordersCancel", "Orders.cancel", cancel)
reg.WriteActionManifest("rsc-host-actions.json") // before each build
```

The build reads the manifest and writes `server-actions.generated.ts` beside
the app, exporting `ordersCancel`; a client component imports and calls it,
and the call arrives here as `Orders.cancel`. Write the file as part of the
build rather than by hand — a stale map names a function that has since been
renamed, and nothing fails until the browser calls it.

`rsckit.Revalidate(ctx, "orders")` inside an action marks a region stale, so
the answer carries it re-rendered instead of the browser being told to ask
again.

## Batches

Calls the renderer issued in one tick of a render — sibling components each
awaiting `rpc()` — arrive as one POST and are answered in order, each with
the status it would have had alone. `CallbackHandler` does this; a function
never sees the difference, and each call keeps its own `Revalidate`.

## What a function sees

- `args.Bind(&a, &b)` decodes positional arguments. Too few is an error;
  extra ones are ignored.
- `rsckit.HeadersFrom(ctx)` has the forwarded `Cookie` and `Authorization`.
  Empty during a build-time render, which has no visitor.
- A panic becomes an error for that one call. It does not take down the
  server, and every other render in flight survives it.

## Go in front

The renderer can face the internet and forward what it does not own to
`RSC_BACKEND` — a Go route, a webhook, an upload. Or Go faces it:
`NewRenderer(url)` is a streaming reverse proxy, `NewHandler` routes the
callback path to the endpoint and everything else to it, and both honour the
markers that keep a url neither side owns from bouncing between them.

## Tests

`go test ./...` covers the contract from this side. The end-to-end proof — a
real page rendered by the engine with data, guards and actions from
`examples/hostserver` — lives with the engine in
`packages/core/tests/js/goAdapter*.test.ts`, which build the example, run it
and render against it. An example that is executed is an example that cannot
drift.
