package rsckit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// FlashCookie is the cookie Flash writes and @rsc-kit/core's useFlash() reads.
// The engine's name, so a message left here and one left by a JavaScript
// action's flash() are the same message.
const FlashCookie = "rsc-flash"

// FlashKind is how useFlash() hands a message to the page: a toast's colour.
type FlashKind string

const (
	FlashSuccess FlashKind = "success"
	FlashError   FlashKind = "error"
	FlashInfo    FlashKind = "info"
	FlashWarning FlashKind = "warning"
)

type flashMessage struct {
	Message string    `json:"message"`
	Kind    FlashKind `json:"kind"`
}

// Flash leaves a message for the next page the visitor sees, shown once by
// useFlash():
//
//	rsckit.Flash(ctx, rsckit.FlashSuccess, "Invoice sent.")
//
// It is a cookie on the answer to this call, put on the page's response as a
// login's is, so it is set from an action or a guard - not from a batched read,
// whose headers have already gone. A message the visitor still carries, and
// one left earlier in the same call, keep their places ahead of it.
//
// The browser reads the cookie, so it is not HttpOnly: nothing secret goes in
// a flash. A kind that is not one of the four reads as FlashInfo, as it does
// in the browser.
func Flash(ctx context.Context, kind FlashKind, message string) {
	switch kind {
	case FlashSuccess, FlashError, FlashInfo, FlashWarning:
	default:
		kind = FlashInfo
	}

	messages := carriedFlash(ctx)

	// One left earlier in this call is the newest list: take it up, and put
	// the whole list back as one cookie.
	if box, ok := ctx.Value(cookieKey).(*cookieBox); ok {
		box.mu.Lock()
		kept := box.cookies[:0]

		for _, c := range box.cookies {
			if c.Name == FlashCookie {
				messages = readFlash(c.Value)
			} else {
				kept = append(kept, c)
			}
		}

		box.cookies = kept
		box.mu.Unlock()
	}

	encoded, _ := json.Marshal(append(messages, flashMessage{Message: message, Kind: kind}))

	SetCookie(ctx, &http.Cookie{
		Name:     FlashCookie,
		Value:    encodeURIComponent(string(encoded)),
		Path:     "/",
		MaxAge:   60,
		SameSite: http.SameSiteLaxMode,
	})
}

// carriedFlash is what the visitor's own cookie still holds: a message from a
// page they have not reached yet.
func carriedFlash(ctx context.Context) []flashMessage {
	headers := HeadersFrom(ctx)
	if headers == nil {
		return nil
	}

	cookie, err := (&http.Request{Header: headers}).Cookie(FlashCookie)
	if err != nil {
		return nil
	}

	return readFlash(cookie.Value)
}

// readFlash keeps only well-formed messages, as the browser does.
func readFlash(value string) []flashMessage {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return nil
	}

	var raw []flashMessage
	if json.Unmarshal([]byte(decoded), &raw) != nil {
		return nil
	}

	messages := make([]flashMessage, 0, len(raw))

	for _, m := range raw {
		if m.Message == "" {
			continue
		}

		switch m.Kind {
		case FlashSuccess, FlashError, FlashInfo, FlashWarning:
		default:
			m.Kind = FlashInfo
		}

		messages = append(messages, m)
	}

	return messages
}

// encodeURIComponent is JavaScript's: what useFlash() decodes with
// decodeURIComponent, which reads %20 and not QueryEscape's +. Every byte left
// is one a cookie value may hold.
func encodeURIComponent(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
