# rsc-kit for Go

Host an rsc-kit application from a Go server.

Go owns the request — sessions, auth, the database. A JS process owns
rendering, because that half is React and there is no way around it. A server
component reaches Go by calling `rpc()`, which arrives here as an ordinary
POST.

```go
registry := rsckit.NewRegistry()

registry.Register("Orders.recent", func(ctx context.Context, args rsckit.Args) (any, error) {
    var limit int
    if err := args.Bind(&limit); err != nil {
        return nil, err
    }

    // The visitor's own cookie, forwarded from the page request — so this
    // query runs as them, not as nobody.
    session := rsckit.HeadersFrom(ctx).Get("Cookie")

    return db.RecentOrders(ctx, session, limit)
})

callback, err := rsckit.NewCallbackHandler(registry, os.Getenv("RSC_HOST_SECRET"))
renderer, err := rsckit.NewRenderer("http://127.0.0.1:5173")

http.ListenAndServe(":8080", rsckit.NewHandler(renderer, callback, "/__rsc/host-call"))
```

The JS side is four lines:

```ts
import { createRscHandler } from '@rsc-kit/core/host'
import { httpHostCalls } from '@rsc-kit/core/host-calls'
import * as engine from './dist/rsc/index.js'

const handle = createRscHandler({
  engine,
  hostCalls: httpHostCalls({
    endpoint: 'http://127.0.0.1:8080/__rsc/host-call',
    secret: process.env.RSC_HOST_SECRET!,
  }),
})
```

## Why there is no frame protocol here

The Laravel host speaks a binary framing over two unix sockets, and a second
channel exists purely so a render can call back into PHP mid-flight
(`PROTOCOL.md`, "The callback channel"). Reimplementing that was the price of a
new host, and it is not the expensive part — the expensive part is everything
around it: partial-navigation depth arithmetic, redirect delivery, cookie
forwarding, prerendered segment variants, PPR. `@rsc-kit/core/host` already
implements all of that, so this adapter does not.

What it costs: a JS process alongside the Go binary. If you want one static
artifact, that is only reachable when every route is prerendered — then the
renderer is a build-time dependency and Go serves files.

## Two things to get right

**The callback endpoint is not public.** It runs functions by name, with none
of the app's routing or authorization in front of it. `NewCallbackHandler`
refuses to be built without a shared secret, and checks it in constant time.
Mounting it on a separate listener bound to loopback is safer still — use
`CallbackHandler` on its own for that, rather than `Handler`.

**Register at startup, once.** `Register` panics on a duplicate name rather
than overwriting. A silent overwrite survives a refactor and then answers the
wrong query.

## Refusing the input

Validation lives where the data is. Answer with fields, not with a failure:

```go
registry.Register("Orders.create", func(ctx context.Context, args rsckit.Args) (any, error) {
    var input Order
    if err := args.Bind(&input); err != nil {
        return nil, err
    }

    if strings.TrimSpace(input.Name) == "" {
        return nil, rsckit.InvalidField("name", "The name field is required.")
    }

    return db.CreateOrder(ctx, input)
})
```

The action needs no error handling of its own:

```ts
export const createOrder = action.handler(({ input }) => rpc('Orders.create', input))
```

`useForm` puts each message under its own input. The shape is field name to
messages — the same one Laravel's `$e->errors()` produces — with a nested field
dot-joined (`address.city`) and a message about the form rather than any one
field under the empty string.

A refusal answers 422 and carries `validationErrors`; a real failure answers
500 and carries `error`. They are separate fields so neither side has to parse
a message to tell an invalid form from a broken server.

## What a function sees

- `args.Bind(&a, &b)` decodes positional arguments. Too few is an error;
  extra ones are ignored.
- `rsckit.HeadersFrom(ctx)` has the forwarded `Cookie` and `Authorization`.
  Empty during a build-time render, which has no visitor.
- `rsckit.Revalidate(ctx, "orders")` marks a region stale, so the answer to an
  action carries the re-rendered part instead of telling the browser to ask
  again.
- A panic becomes an error for that one call. It does not take down the server,
  and every other render in flight survives it.

## Running the example

```
go run ./examples/hostserver -secret s3cret -addr 127.0.0.1:8099
```

It is also the fixture for the end-to-end tests in
`packages/core/tests/js/goAdapter*.test.ts`, which build it, run it, and render
a real page whose data comes from it — an example that is executed is an
example that cannot drift.
