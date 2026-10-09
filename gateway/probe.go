package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type check struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	OK     bool     `json:"ok"`
	MS     int64    `json:"ms"`
	Detail string   `json:"detail,omitempty"`
	Error  string   `json:"error,omitempty"`
	Env    []string `json:"env"`
	Hops   []string `json:"hops"`
}

type variable struct {
	Name    string `json:"name"`
	FP      string `json:"fp,omitempty"`
	Value   string `json:"value,omitempty"`
	Role    string `json:"role"`
	Peer    string `json:"peer,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// peer is a verifier this project calls with a shared key.
type peer struct{ name, urlVar, keyVar string }

var peers = []peer{
	{"catalog-api", "CATALOG_URL", "INTERNAL_TOKEN"},
	{"rails-queue", "RAILS_URL", "WEBHOOK_SECRET"},
}

// workerChecks are run by the orders worker, which holds the PG and Redis clients.
var workerChecks = []check{
	{ID: "postgres", Label: "Postgres write, read, delete", Env: []string{"DATABASE_URL", "ORDERS_URL"}},
	{ID: "redis", Label: "Redis SET/GET/DEL with TTL", Env: []string{"REDIS_URL", "ORDERS_URL"}},
	{ID: "fulfiller", Label: "Fulfiller worker heartbeat in Redis", Env: []string{"REDIS_URL", "ORDERS_URL"}},
}

func probeVars() []variable {
	var out []variable
	for _, v := range []variable{
		{Name: "INTERNAL_TOKEN", Role: "signs"}, {Name: "WEBHOOK_SECRET", Role: "signs"},
		{Name: "CATALOG_URL", Role: "url", Peer: "catalog-api"}, {Name: "RAILS_URL", Role: "url", Peer: "rails-queue"},
		{Name: "ORDERS_URL", Role: "plain"}, {Name: "STOCK_URL", Role: "plain"}, {Name: "ZOO_PANEL_ORIGIN", Role: "plain"},
	} {
		switch val := os.Getenv(v.Name); {
		case val == "":
			v.Missing = true
		case v.Role == "signs":
			v.FP = fp(val)
		default:
			v.Value = val
		}
		out = append(out, v)
	}
	return out
}

func (g *gateway) runWorkerChecks(ctx context.Context, me string) []check {
	t0 := time.Now()
	code, data, err := g.call(ctx, 5*time.Second, http.MethodGet, g.orders+"/checks", nil, nil)
	var got []check
	if err == nil && code != http.StatusOK {
		err = fmt.Errorf("orders worker answered %d", code)
	}
	if err == nil {
		err = json.Unmarshal(data, &got)
	}
	if err != nil {
		got = append([]check(nil), workerChecks...)
		for i := range got {
			got[i].MS, got[i].Error = time.Since(t0).Milliseconds(), "orders worker: "+err.Error()
		}
	}
	for i := range got {
		got[i].Hops = []string{me}
	}
	return got
}

func (g *gateway) workerHealth(ctx context.Context, me, worker, urlVar, base string) check {
	c := check{ID: "worker:" + worker, Label: "Port worker " + worker + " health via " + urlVar, Env: []string{urlVar}, Hops: []string{me}}
	t0 := time.Now()
	code, data, err := g.call(ctx, 5*time.Second, http.MethodGet, base+"/health", nil, nil)
	c.MS = time.Since(t0).Milliseconds()
	var body struct{ Detail string }
	json.Unmarshal(data, &body)
	switch {
	case err != nil:
		c.Error = err.Error()
	case code != http.StatusOK:
		c.Error = fmt.Sprintf("status %d", code)
	default:
		c.OK, c.Detail = true, body.Detail
	}
	return c
}

// verifyPeer calls the peer's /_zoo/verify signed and checks name, key fp and public_url.
func (g *gateway) verifyPeer(ctx context.Context, me string, p peer, now time.Time) check {
	base, key := strings.TrimRight(os.Getenv(p.urlVar), "/"), os.Getenv(p.keyVar)
	c := check{ID: "peer:" + p.name, Label: "Signed verify call to " + p.name, Env: []string{p.urlVar, p.keyVar}, Hops: []string{me, p.name + "@local"}}
	if u, err := url.Parse(base); err == nil {
		c.Hops[1] = p.name + "@" + serverLabel(u.Hostname())
	}
	for _, k := range []string{p.urlVar, p.keyVar} {
		if os.Getenv(k) == "" {
			c.Error = k + " is not set"
			return c
		}
	}
	t0 := time.Now()
	hdr := http.Header{"X-Zoo-Signature": {sigHeader(key, now.Unix(), "GET", "/_zoo/verify", nil)}}
	code, data, err := g.call(ctx, 8*time.Second, http.MethodGet, base+"/_zoo/verify", nil, hdr)
	c.MS = time.Since(t0).Milliseconds()
	var v struct {
		Name, Error string
		PublicURL   string `json:"public_url"`
		KeyFP       string `json:"key_fp"`
		VerifiedBy  string `json:"verified_by"`
	}
	if err == nil {
		json.Unmarshal(data, &v)
	}
	switch {
	case err != nil:
		c.Error = err.Error()
	case code != http.StatusOK:
		c.Error = fmt.Sprintf("status %d: %s", code, v.Error)
	case v.Name != p.name:
		c.Error = fmt.Sprintf("name %q, want %q", v.Name, p.name)
	case v.KeyFP != fp(key):
		c.Error = fmt.Sprintf("key_fp mismatch: peer %s, ours %s", v.KeyFP, fp(key))
	case strings.TrimRight(v.PublicURL, "/") != base:
		c.Error = fmt.Sprintf("public_url %s does not match %s %s", v.PublicURL, p.urlVar, base)
	default:
		c.OK, c.Detail = true, fmt.Sprintf("verified by %s, key fp %s", v.VerifiedBy, v.KeyFP)
	}
	return c
}

func (g *gateway) probe(w http.ResponseWriter, r *http.Request) {
	select {
	case g.probeSlot <- struct{}{}:
	case <-time.After(5 * time.Second):
		fail(w, http.StatusTooManyRequests, "probe busy")
		return
	case <-r.Context().Done():
		return
	}
	defer func() { <-g.probeSlot }()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	t0, now := time.Now(), g.now()
	me := name + "@" + serverLabel(os.Getenv("PUBLIC_HOST"))
	steps := []func() []check{
		func() []check { return g.runWorkerChecks(ctx, me) },
		func() []check { return []check{g.workerHealth(ctx, me, "orders", "ORDERS_URL", g.orders)} },
		func() []check { return []check{g.workerHealth(ctx, me, "stock", "STOCK_URL", g.stock)} },
	}
	for _, p := range peers {
		steps = append(steps, func() []check { return []check{g.verifyPeer(ctx, me, p, now)} })
	}
	results := make([][]check, len(steps))
	var wg sync.WaitGroup
	for i, f := range steps {
		wg.Go(func() { results[i] = f() })
	}
	wg.Wait()
	checks, ok := []check{}, true
	for _, rs := range results {
		for _, c := range rs {
			ok = ok && c.OK
			checks = append(checks, c)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "stack": stack, "server": serverLabel(os.Getenv("PUBLIC_HOST")), "release": release(),
		"env": envOr("OX_ENV", "local"), "ok": ok, "ms": time.Since(t0).Milliseconds(),
		"at": now.UTC().Format(time.RFC3339), "checks": checks, "vars": probeVars(),
	})
}
