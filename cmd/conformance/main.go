// Command conformance serves the Conformance.* functions rsc-kit's suite
// calls, written with this package's ordinary API, so the suite checks what
// an app built on it would send:
//
//	go run ./cmd/conformance -addr 127.0.0.1:8123 -secret test -manifest rsc-host.json &
//	npx -y -p @rsc-kit/core rsc-kit-conformance --endpoint http://127.0.0.1:8123/__rsc/host-call --secret test --manifest rsc-host.json
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"time"

	rsckit "github.com/rsc-kit/go"
)

// Row is what Conformance.emptyList has none of.
type Row struct {
	ID int `json:"id"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8123", "where to listen")
	secret := flag.String("secret", "test", "the shared secret")
	manifest := flag.String("manifest", "", "write rsc-host.json here before serving")
	flag.Parse()

	reg := rsckit.NewRegistry()

	reg.Handle("Conformance.echo", func(v any) any { return v })

	reg.Handle("Conformance.emptyList", func() ([]Row, error) {
		var none []Row // "no rows", the way Go writes it

		return none, nil
	})

	reg.Handle("Conformance.time", func() time.Time {
		return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	})

	reg.Handle("Conformance.noTime", func() *time.Time { return nil })

	reg.Handle("Conformance.unauthenticated", func() error { return rsckit.Unauthenticated() })
	reg.Handle("Conformance.unauthorized", func() error { return rsckit.Unauthorized() })
	reg.Handle("Conformance.notFound", func() error { return rsckit.Refuse(http.StatusNotFound, "Not found.") })
	reg.Handle("Conformance.refuse", func() error { return rsckit.Refuse(http.StatusTooManyRequests, "Slow down.") })
	reg.Handle("Conformance.invalid", func() error { return rsckit.InvalidField("name", "The name field is required.") })
	reg.Handle("Conformance.redirect", func() error { return rsckit.Redirect("/login") })

	reg.Handle("Conformance.revalidate", func(ctx context.Context) string {
		rsckit.Revalidate(ctx, "orders")

		return "ok"
	})

	reg.Handle("Conformance.fail", func() error { return errors.New("boom") })

	reg.Handle("Conformance.authorization", func(ctx context.Context) string {
		return rsckit.HeadersFrom(ctx).Get("Authorization")
	})

	reg.Middleware("conformance-allow", func(context.Context, string) error { return nil })
	reg.Middleware("conformance-deny", func(context.Context, string) error { return rsckit.Unauthorized() })

	if *manifest != "" {
		if err := reg.WriteManifest(*manifest); err != nil {
			log.Fatal(err)
		}
	}

	callback, err := rsckit.NewCallbackHandler(reg, *secret)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/__rsc/host-call", callback)

	log.Printf("conformance fixture on http://%s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
