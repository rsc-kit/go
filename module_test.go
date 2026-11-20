package rsckit

import (
	"context"
	"strings"
	"testing"
)

// The same import an app reads from Laravel: the class in kebab case, a
// dotted prefix as folders, and a name with no method the default.
func TestAnActionIsAModulePerClass(t *testing.T) {
	cases := map[string][2]string{
		"Orders.create":         {"orders", "create"},
		"SendInvite":            {"send-invite", "default"},
		"Billing.Invoices.send": {"billing/invoices", "send"},
		"OAuthLogin.start":      {"o-auth-login", "start"},
		"Orders.delete":         {"orders", "delete"},
	}

	for name, want := range cases {
		module, export := actionModule(name)
		if module != want[0] || export != want[1] {
			t.Errorf("%s = %s, %s; want %s, %s", name, module, export, want[0], want[1])
		}
	}
}

func TestOneExportTwiceIsRefused(t *testing.T) {
	reg := NewRegistry()
	noop := func(context.Context, Args) (any, error) { return nil, nil }

	reg.RegisterAction("Orders.cancel", noop)

	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "cancel in server-actions/orders") {
			t.Fatalf("recover() = %v", r)
		}
	}()

	reg.RegisterAction("orders.cancel", noop)
}
