package rsckit

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// A form asking whether its fields would pass before it is submitted. The
// function validates as it always does and stops before writing; one that
// never said it would is never sent a check.

type subscribeForm struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func subscribing(writes *int) func(context.Context, subscribeForm) (any, error) {
	return func(ctx context.Context, in subscribeForm) (any, error) {
		errs := map[string][]string{}
		if !strings.Contains(in.Email, "@") {
			errs["email"] = []string{"Not an email."}
		}
		if in.Name == "" {
			errs["name"] = []string{"Required."}
		}
		if len(errs) > 0 {
			return nil, Invalid(errs)
		}

		if _, checking := Checking(ctx); checking {
			return nil, nil
		}

		*writes++

		return map[string]string{"subscribed": in.Email}, nil
	}
}

func checkable(t *testing.T, writes *int) *CallbackHandler {
	return handler(t, func(r *Registry) {
		r.HandleAction("Newsletter.subscribe", subscribing(writes))
		r.Validates("Newsletter.subscribe")
	})
}

func TestACheckAnswersTheValidationErrorsAndWritesNothing(t *testing.T) {
	writes := 0
	rec := post(checkable(t, &writes), `{"function":"Newsletter.subscribe","args":[{"email":"nope","name":""}],"validate":["email"]}`, nil)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", rec.Code, rec.Body)
	}

	// Every field the function refused: the renderer keeps the ones asked.
	if got := decode(t, rec).ValidationErrors["email"]; !reflect.DeepEqual(got, []string{"Not an email."}) {
		t.Fatalf("email errors %v", got)
	}

	if writes != 0 {
		t.Fatalf("a check wrote %d time(s)", writes)
	}
}

func TestAPassingCheckIsANullResult(t *testing.T) {
	writes := 0
	rec := post(checkable(t, &writes), `{"function":"Newsletter.subscribe","args":[{"email":"a@b.co","name":"Ada"}],"validate":[]}`, nil)

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("status %d body %s, want 200 {}", rec.Code, rec.Body)
	}

	if writes != 0 {
		t.Fatalf("a check wrote %d time(s)", writes)
	}
}

func TestCheckingCarriesTheFields(t *testing.T) {
	var seen []string
	var asked bool

	h := handler(t, func(r *Registry) {
		r.Register("Profile.update", func(ctx context.Context, _ Args) (any, error) {
			seen, asked = Checking(ctx)

			return nil, nil
		})
		r.Validates("Profile.update")
	})

	post(h, `{"function":"Profile.update","args":[],"validate":["items.*.sku"]}`, nil)

	if !asked || !reflect.DeepEqual(seen, []string{"items.*.sku"}) {
		t.Fatalf("Checking gave %v, %v", seen, asked)
	}

	post(h, `{"function":"Profile.update","args":[]}`, nil)

	if asked {
		t.Fatal("an ordinary call read as a check")
	}
}

func TestASubmitStillRuns(t *testing.T) {
	writes := 0
	rec := post(checkable(t, &writes), `{"function":"Newsletter.subscribe","args":[{"email":"a@b.co","name":"Ada"}]}`, nil)

	if rec.Code != http.StatusOK || writes != 1 {
		t.Fatalf("status %d writes %d", rec.Code, writes)
	}
}

// A function that never asks Checking would run the call.
func TestAFunctionNotNamedInValidatesIsNeverChecked(t *testing.T) {
	ran := false
	h := handler(t, func(r *Registry) {
		r.Register("Orders.create", func(context.Context, Args) (any, error) {
			ran = true

			return nil, nil
		})
	})

	rec := post(h, `{"function":"Orders.create","args":[],"validate":["name"]}`, nil)

	if rec.Code != http.StatusBadRequest || ran {
		t.Fatalf("status %d ran %v, want 400 and not run", rec.Code, ran)
	}
}

func TestACheckInABatchIsRefused(t *testing.T) {
	writes := 0
	rec := post(checkable(t, &writes), `{"calls":[{"function":"Newsletter.subscribe","args":[{"email":"a@b.co","name":"Ada"}],"validate":["email"]}]}`, nil)

	if !strings.Contains(rec.Body.String(), `"status":400`) || writes != 0 {
		t.Fatalf("body %s writes %d", rec.Body, writes)
	}
}

func TestTheManifestListsWhatCanBeChecked(t *testing.T) {
	reg := NewRegistry()
	reg.HandleAction("Newsletter.subscribe", subscribing(new(int)))
	reg.Register("Orders.create", func(context.Context, Args) (any, error) { return nil, nil })
	reg.Validates("Newsletter.subscribe")

	if got := reg.Manifest().Validates; !reflect.DeepEqual(got, []string{"Newsletter.subscribe"}) {
		t.Fatalf("validates %v", got)
	}
}

func TestValidatesPanicsOnANameNotRegistered(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()

	NewRegistry().Validates("Nobody.home")
}
