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
if err != nil {
    log.Fatal(err) // refuses to exist without a secret
}
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

The JavaScript side is what `bunx rsc-kit@latest init` writes in the
directory with `go.mod` (or `bun create rsc-kit@latest my-app
--backend=http://127.0.0.1:8080` for a new app): the route tree,
`vite.config.ts`, the scripts, and two lines in `.env`:

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
[docs.rsc-kit.dev/hosts/your-own-backend](https://docs.rsc-kit.dev/hosts/your-own-backend).

What it costs: a JS process alongside the Go binary. If you want one static
artifact, that is only reachable when every route is prerendered — then the
renderer is a build-time dependency and Go serves files.

## Two things to get right

**The callback endpoint is not public.** It runs functions by name, with none
of the app's routing or authorization in front of it. `NewCallbackHandler`
refuses to be built without a shared secret, and checks it in constant time.
Restrict the path at the web server as well, or mount it on a separate
listener bound to loopback.

**Register at startup, once.** `Handle`, `Register`, their action forms and
`Middleware` panic on a duplicate name rather than overwriting. A silent
overwrite survives a refactor and then answers the wrong query. `Handle` also
panics on a value that is not a function it can call, and `Register` on the
reserved `__rsc.middleware`.

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

In development, set `Debug` on the `CallbackHandler`, or run with
`RSC_DEBUG=1`, and a 500 also carries where in Go it failed: the error's type,
and for a panic the frames it unwound. The renderer shows them under its own
stack as the error's cause. Never in production: a trace names your files.
A refusal never carries one.

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

## Saying data changed

A section that says what it refreshes on refreshes in every open tab the
moment this side says it changed — from a webhook, a job, anywhere with a
context, with nothing polling:

```ts
// app/t/[team]/repos.section.tsx
export default section('repos', Repos, { refreshOn: ({ params }) => [`team:${params.team}:repos`] })
```

```go
// in the GitHub webhook handler
reg.Changed(ctx, "team:"+teamID+":repos")
```

A name has a version that moves when `Changed` names it; nothing else
travels. The renderer asks `__rsc.changed` for the versions that moved since
the ones a tab holds, and the registry holds that call until one does — woken
at once by a `Changed` in this process, checking the store every
`rsckit.ChangedPoll` (1s) for one made by another instance — or the renderer's
wait runs out.

Versions live in a `VersionStore`. The default is in memory, which is one
instance. With more than one, a webhook lands on whichever instance the
balancer picked, so give every instance the same store:

```go
// CREATE TABLE rsc_versions (name TEXT PRIMARY KEY, version BIGINT NOT NULL)
reg.Versions(&rsckit.SQLVersions{DB: db, Placeholder: rsckit.Dollar})   // Postgres; nil Placeholder is "?"
```

Anything with a `Bump` and a `Versions` fits — Redis, a cache.

That shared store is noticed within `ChangedPoll`, a read a second per held
call. To hear another instance's change the moment it is made — and not read
the store while nothing changes — have the store announce it and every
instance listen. On Postgres, `Notify` sends a `pg_notify` after each bump,
and `WakeOn` runs your listener; the adapter stays free of a driver, so the
listening is ten lines of yours, here with pgx:

```go
reg.Versions(&rsckit.SQLVersions{DB: db, Placeholder: rsckit.Dollar, Notify: "rsc_versions"})

reg.WakeOn(ctx, func(ctx context.Context, connected, wake func()) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN rsc_versions"); err != nil {
		return err
	}
	connected()
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		wake()
	}
})
```

The notification carries nothing — it only says "ask". A listener that drops
is run again with backoff, and asks once on its return; while it is down,
the poll is back, so nothing is missed. LISTEN needs a session: through
PgBouncer in transaction mode, connect the listener directly to Postgres.
Redis pub/sub or a broadcast server fit the same way: call `wake` on each
message. One instance needs none of this — its own `Changed` already wakes it.

A version is when the name last changed, in milliseconds — `NextVersion`, the
larger of one past the old value and now — so it never repeats, and old names
can be deleted at any time: a tab still holding one refreshes once. Prune on a
schedule, since a row is kept for every name that ever changed:

```go
store.Prune(ctx, 0) // names not changed in rsckit.KeepVersions (30 days); or any duration
```

`MemoryVersions` forgets old names on its own (`ForgetAfter`). A `VersionStore`
of your own should bump with `rsckit.NextVersion`, not `+ 1`, before you prune
it.

## Typed functions

`Handle` and `HandleAction` take an ordinary Go function: any number of
parameters of any type JSON can carry, bound from `rpc()`'s arguments in
order, with an optional leading `context.Context`. A trailing pointer may be
left out by the caller; a variadic parameter takes the rest. It returns a
value and an error, only an error, or only a value.

The parameter and result types go into `rsc-host.json` as JSON Schema, and
the build turns them into TypeScript: each struct an interface named for its
Go type, by its `json` names. `omitempty` is optional, a pointer is `| null`,
`time.Time` is a date-time string and `*time.Time` a string or `null`; a type
with its own `MarshalJSON` is `unknown`, since its shape is its own. A nil
slice or map in a typed function's result is sent as `[]` or `{}`, as its type
says, never `null`; a nil pointer is `null`. `Register` and `RegisterAction` keep the untyped
`func(ctx, rsckit.Args) (any, error)` form.

## Server actions

```go
reg.HandleAction("ordersCancel", "Orders.cancel", func(ctx context.Context, id int) error { … })
reg.WriteManifest("rsc-host.json")
```

A form can post to an action directly; its fields arrive as the first
parameter, decoded into the struct.

`rsc-host.json` has `actions`, `functions`, and the `types` and `defs` of
what `Handle` registered. The build
writes `server-actions.generated.ts` beside the app, exporting `ordersCancel`;
a client component imports and calls it, and the call arrives here as
`Orders.cancel`. The function names become the type of `rpc()`'s first
argument, so `rpc('Orders.recnet')` fails the typecheck.

Have the build write it, so it cannot go stale. Give your binary a flag that
writes the manifest and exits, and name it in the Vite config:

```ts
rscKit({
  hostManifest: {
    command: ['go', 'run', '.', '-manifest', '../rsc-host.json'],
    cwd: 'backend',      // where the command runs; default the project root
    watch: ['backend'],  // dev runs it again when the Go source changes
  },
})
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
`NewRenderer(url)` is a streaming reverse proxy (`NewUnixRenderer(path)` for
a renderer on a unix socket), `NewHandler(renderer, callback, path)` routes the
callback path to the endpoint and everything else to it, and both honour the
markers that keep a url neither side owns from bouncing between them.

## Conformance

`cmd/conformance` serves the functions rsc-kit's conformance suite calls,
written with this package's ordinary API, and CI runs the suite against it on
every change: empty lists, times, every refusal, not-found, redirects,
revalidation, batches, guards and the secret, with each value checked against
the type the manifest declares. To run it locally:

```sh
go run ./cmd/conformance -manifest /tmp/rsc-host.json &
npx -y -p @rsc-kit/core rsc-kit-conformance \
  --endpoint http://127.0.0.1:8123/__rsc/host-call --secret test --manifest /tmp/rsc-host.json
```

## Tests

`go test ./...` covers the contract from this side. The end-to-end proof — a
real page rendered by the engine with data, guards, actions and batches from a
Go server — lives with the engine, in
[`packages/core/tests/js/goAdapter*.test.ts`](https://github.com/rsc-kit/rsc-kit/tree/main/packages/core/tests/js),
which build a host server on this module, run it and render against it. A
contract change is released here first, and the engine's fixture follows.

Guides, the contract every backend answers, and a runnable example at
[docs.rsc-kit.dev/hosts/go](https://docs.rsc-kit.dev/hosts/go).
