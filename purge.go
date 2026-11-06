package rsckit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Purging: telling the CDN in front that kept pages changed.
//
// A revalidate page goes out with the Cache-Tag of every cache() answer it
// used, and the names Changed moves are those tags. The change is made here
// and never passes through the renderer, so this side tells the CDN itself:
// on Homeport, where HOMEPORT_PURGE_URL is set, Changed posts
//
//	POST $HOMEPORT_PURGE_URL  {"tags": ["team:1:repos"]}
//
// once the version has moved. Inside a host call, every name the call changed
// goes out as one purge after its answer has been written; elsewhere - a
// webhook, a job - in the background at once. Call Changed after your
// transaction commits: a CDN that fetches the page between the purge and the
// commit keeps the old data. A purge that fails is logged and never fails the
// change: the CDN still drops its copy at s-maxage.

// Purge is what a purge drops: pages carrying any of Tags, pages at Paths, or
// everything.
type Purge struct {
	Tags  []string `json:"tags,omitempty"`
	Paths []string `json:"paths,omitempty"`
	All   bool     `json:"all,omitempty"`
}

// Purger sends a purge to a CDN.
type Purger func(ctx context.Context, purge Purge) error

// PurgeLimit is the most tags and paths, together, Homeport takes in one call.
const PurgeLimit = 100

// The longest tag and path Homeport takes, in bytes.
const (
	purgeTagBytes  = 1024
	purgePathBytes = 2048
)

// SetPurger installs the purge Changed calls. nil purges nothing, Homeport's
// default included; without a call, the purge is HomeportPurger when
// HOMEPORT_PURGE_URL is set.
func (r *Registry) SetPurger(purger Purger) {
	r.purgeMu.Lock()
	defer r.purgeMu.Unlock()

	r.purger = purger
	r.purgerSet = true
}

func (r *Registry) currentPurger() Purger {
	r.purgeMu.Lock()
	defer r.purgeMu.Unlock()

	if r.purgerSet {
		return r.purger
	}

	if url := os.Getenv("HOMEPORT_PURGE_URL"); url != "" {
		return HomeportPurger(url, nil)
	}

	return nil
}

// HomeportPurger posts to Homeport's purge url: JSON, no token - it is
// reachable only from inside the app's network - in calls of at most
// PurgeLimit tags and paths together. A tag or path too long for it is left
// out: one would cost the whole call, and the edge keeps no page with a tag it
// could not purge. Only a 202 is accepted. A 429 is tried once more after its
// Retry-After (at most five seconds). client nil is a client with a
// five-second timeout.
func HomeportPurger(url string, client *http.Client) Purger {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}

	return func(ctx context.Context, purge Purge) error {
		var bodies []Purge

		if purge.All {
			bodies = append(bodies, Purge{All: true})
		}

		tags, paths := fitting(purge.Tags, purgeTagBytes), fitting(purge.Paths, purgePathBytes)

		for len(tags) > 0 || len(paths) > 0 {
			var body Purge

			body.Tags, tags = take(tags, PurgeLimit)
			body.Paths, paths = take(paths, PurgeLimit-len(body.Tags))
			bodies = append(bodies, body)
		}

		for _, body := range bodies {
			if err := postPurge(ctx, client, url, body, false); err != nil {
				return err
			}
		}

		return nil
	}
}

func take(list []string, n int) ([]string, []string) {
	if len(list) <= n {
		return list, nil
	}

	return list[:n], list[n:]
}

func fitting(list []string, limit int) []string {
	var fit []string

	for _, item := range list {
		if len(item) <= limit {
			fit = append(fit, item)
		}
	}

	return fit
}

func postPurge(ctx context.Context, client *http.Client, url string, body Purge, retried bool) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return err
	}

	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()

	if response.StatusCode == http.StatusAccepted {
		return nil
	}

	if response.StatusCode == http.StatusTooManyRequests && !retried {
		seconds, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		seconds = min(max(seconds, 1), 5)

		select {
		case <-time.After(time.Duration(seconds) * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}

		return postPurge(ctx, client, url, body, true)
	}

	return fmt.Errorf("the CDN answered %d to a purge of %s", response.StatusCode, payload)
}

// pendingPurge gathers the names one host call changed, purged once it has answered.
type pendingPurge struct {
	mu   sync.Mutex
	tags []string
	seen map[string]bool
}

func (p *pendingPurge) add(names []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.seen == nil {
		p.seen = make(map[string]bool)
	}

	for _, name := range names {
		if !p.seen[name] {
			p.seen[name] = true
			p.tags = append(p.tags, name)
		}
	}
}

func (p *pendingPurge) take() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	tags := p.tags
	p.tags, p.seen = nil, nil

	return tags
}

type purgeKeyType struct{}

var purgeKey purgeKeyType

// purgeLater sends a purge of these names: after the host call in ctx has
// answered, or in the background at once outside one.
func (r *Registry) purgeLater(ctx context.Context, names []string) {
	if len(names) == 0 || r.currentPurger() == nil {
		return
	}

	if pending, ok := ctx.Value(purgeKey).(*pendingPurge); ok {
		pending.add(names)

		return
	}

	go r.purgeNow(context.WithoutCancel(ctx), names)
}

func (r *Registry) purgeNow(ctx context.Context, tags []string) {
	purger := r.currentPurger()
	if purger == nil || len(tags) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := purger(ctx, Purge{Tags: tags}); err != nil {
		logf("rsckit: purging the CDN failed; its copy is dropped at s-maxage instead: %v", err)
	}
}
