package rsckit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

// The flash cookie as useFlash() reads it: decodeURIComponent, then JSON.
func flashOf(t *testing.T, cookies []*http.Cookie) []flashMessage {
	t.Helper()

	for _, c := range cookies {
		if c.Name != FlashCookie {
			continue
		}

		if c.HttpOnly {
			t.Fatal("HttpOnly: useFlash reads document.cookie and would never see it")
		}

		if c.Path != "/" || c.MaxAge <= 0 || c.MaxAge > 60 || c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("attributes %+v", c)
		}

		decoded, err := url.PathUnescape(c.Value)
		if err != nil {
			t.Fatalf("value %q: %v", c.Value, err)
		}

		var messages []flashMessage
		if err := json.Unmarshal([]byte(decoded), &messages); err != nil {
			t.Fatalf("value %q is not JSON: %v", decoded, err)
		}

		return messages
	}

	t.Fatal("no flash cookie on the answer")

	return nil
}

func TestAFlashIsACookieTheBrowserReads(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Handle("Invoices.send", func(ctx context.Context) string {
			Flash(ctx, FlashSuccess, "Invoice sent: 50% paid, \"net 30\"; thanks")

			return "ok"
		})
	})

	got := flashOf(t, post(h, `{"function":"Invoices.send","args":[]}`, nil).Result().Cookies())
	want := []flashMessage{{Message: "Invoice sent: 50% paid, \"net 30\"; thanks", Kind: FlashSuccess}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestAFlashKeepsWhatTheVisitorCarriesAndWhatTheCallLeftEarlier(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Handle("Invoices.send", func(ctx context.Context) string {
			Flash(ctx, FlashSuccess, "Sent.")
			Flash(ctx, FlashError, "The copy bounced.")

			return "ok"
		})
	})

	carried := encodeURIComponent(`[{"message":"Earlier.","kind":"info"}]`)
	rec := post(h, `{"function":"Invoices.send","args":[]}`, map[string]string{"Cookie": "other=1; " + FlashCookie + "=" + carried})

	if n := len(rec.Header().Values("Set-Cookie")); n != 1 {
		t.Fatalf("one flash cookie, not %d: %v", n, rec.Header().Values("Set-Cookie"))
	}

	got := flashOf(t, rec.Result().Cookies())
	want := []flashMessage{{"Earlier.", FlashInfo}, {"Sent.", FlashSuccess}, {"The copy bounced.", FlashError}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestAFlashOfAKindTheBrowserDoesNotKnowIsInfo(t *testing.T) {
	h := handler(t, func(r *Registry) {
		r.Handle("X.y", func(ctx context.Context) string {
			Flash(ctx, "succes", "Saved.")

			return "ok"
		})
	})

	got := flashOf(t, post(h, `{"function":"X.y","args":[]}`, map[string]string{"Cookie": FlashCookie + "=not-json"}).Result().Cookies())

	if !reflect.DeepEqual(got, []flashMessage{{"Saved.", FlashInfo}}) {
		t.Fatalf("got %+v", got)
	}
}

func TestEncodeURIComponentIsJavaScripts(t *testing.T) {
	// The values encodeURIComponent gives for these in a browser.
	for in, want := range map[string]string{
		`a b`:       `a%20b`,
		`[{"k":1}]`: `%5B%7B%22k%22%3A1%7D%5D`,
		`é`:         `%C3%A9`,
		`-_.~`:      `-_.~`,
	} {
		if got := encodeURIComponent(in); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", in, got, want)
		}
	}
}
