// Command conformance serves the Conformance.* functions rsc-kit's suite
// calls, written with this package's ordinary API, so the suite checks what
// an app built on it would send:
//
//	RSC_PURGE_URL=http://127.0.0.1:8790/ RSC_HOST_CALL_SECRET_PREVIOUS=old go run ./cmd/conformance -addr 127.0.0.1:8123 -secret test -manifest rsc-host.json &
//	npx -y -p @rsc-kit/core rsc-kit-conformance --endpoint http://127.0.0.1:8123/__rsc/host-call --secret test --previous-secret old --manifest rsc-host.json --purge-port 8790
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	rsckit "github.com/rsc-kit/go"
)

// Row is what Conformance.emptyList has none of.
type Row struct {
	ID int `json:"id"`
}

type formInput struct {
	Name  string `json:"name" validate:"required,min=2"`
	Email string `json:"email" validate:"required,email"`
	Topic string `json:"topic" validate:"required,oneof=sales support"`
	// Asked only of support requests: a form shows it while topic is support.
	Company string `json:"company,omitempty" validate:"required_if=Topic support"`
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
	reg.Handle("Conformance.refuseWithData", func() error {
		return rsckit.RefuseWith(http.StatusConflict, "Still in use", map[string]any{
			"blockers": []map[string]any{{"id": 7, "href": "/orders/7"}},
		})
	})
	reg.Handle("Conformance.form", func(in formInput) (string, error) {
		if in.Email == "taken@example.com" {
			return "", rsckit.InvalidField("email", "That address is taken.")
		}

		return in.Name, nil
	})
	// An admin panel's update: the values, then which record. The address
	// belongs to record 7, which may keep it, as a unique rule ignoring the
	// record being edited lets it.
	reg.Handle("Conformance.update", func(in formInput, key string) (string, error) {
		if in.Email == "taken@example.com" && key != "7" {
			return "", rsckit.InvalidField("email", "That address is taken.")
		}

		return key + ":" + in.Name, nil
	})
	reg.Handle("Conformance.invalid", func() error { return rsckit.InvalidField("name", "The name field is required.") })
	reg.Handle("Conformance.redirect", func() error { return rsckit.Redirect("/login") })

	reg.Handle("Conformance.revalidate", func(ctx context.Context) string {
		rsckit.Revalidate(ctx, "orders")

		return "ok"
	})

	reg.Handle("Conformance.fail", func() error { return errors.New("boom") })

	reg.Handle("Conformance.change", func(ctx context.Context) (string, error) {
		return "ok", reg.Changed(ctx, "conformance:changed")
	})

	reg.Handle("Conformance.authorization", func(ctx context.Context) string {
		return rsckit.HeadersFrom(ctx).Get("Authorization")
	})

	reg.Handle("Conformance.cookie", func(ctx context.Context) string {
		return strings.Join(rsckit.HeadersFrom(ctx).Values("Cookie"), "; ")
	})

	reg.Handle("Conformance.ip", func(ctx context.Context) string { return rsckit.ClientIP(ctx) })

	reg.Handle("Conformance.userAgent", func(ctx context.Context) string { return rsckit.ClientUserAgent(ctx) })

	reg.Handle("Conformance.login", func(ctx context.Context) string {
		rsckit.SetCookie(ctx, &http.Cookie{Name: "conformance_login", Value: "1", Path: "/", HttpOnly: true})

		return "ok"
	})

	reg.Handle("Conformance.flash", func(ctx context.Context) string {
		rsckit.Flash(ctx, rsckit.FlashSuccess, "Saved.")

		return "ok"
	})

	reg.Handle("Conformance.double", func(n int) int { return n * 2 })

	reg.Handle("Conformance.invalidNested", func() error {
		return rsckit.Invalid(map[string][]string{
			"address.city": {"The city is required."},
			"":             {"The address could not be checked."},
		})
	})

	reg.Middleware("conformance-allow", func(context.Context, string) error { return nil })
	reg.Middleware("conformance-deny", func(context.Context, string) error { return rsckit.Unauthorized() })
	reg.Middleware("conformance-redirect", func(context.Context, string) error { return rsckit.Redirect("/conformance-login") })
	reg.Middleware("conformance-page", func(ctx context.Context, _ string) error {
		page, ok := rsckit.PageFrom(ctx)
		if !ok || page.Params["order"] != "7" {
			return rsckit.Unauthorized()
		}

		if u, err := url.Parse(page.URL); err != nil || u.Path != "/conformance/orders/7" {
			return rsckit.Unauthorized()
		}

		return nil
	})

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
