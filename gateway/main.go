// gateway: mesh-shop's public entry ([app]). It serves /api (proxied to the
// orders and stock workers that ox names in ORDERS_URL and STOCK_URL) and the
// /_zoo contract, aggregating the probe across every process of the project.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	name  = "mesh-shop"
	stack = "Monorepo: Go gateway + Bun orders + Python stock + Node fulfiller + static web"
)

var (
	builtAt   string // set with -ldflags at build time
	startedAt = time.Now().UTC()
	traceRE   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	skuRE     = regexp.MustCompile(`^[A-Z0-9-]{1,32}$`)
	serverRE  = regexp.MustCompile(`^s[0-9]+$`)
	jsonHdr   = http.Header{"Content-Type": {"application/json"}}
)

func serverLabel(host string) string {
	for _, part := range strings.Split(host, ".") {
		if serverRE.MatchString(part) {
			return part
		}
	}
	return "local"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func release() string {
	r := envOr("OX_RELEASE", "unknown")
	return r[:min(len(r), 12)]
}

func fp(v string) string {
	s := sha256.Sum256([]byte(v))
	return hex.EncodeToString(s[:])[60:]
}

func sign(key string, t int64, method, path string, body []byte) string {
	b := sha256.Sum256(body)
	m := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(m, "%d.%s.%s.%s", t, strings.ToUpper(method), path, hex.EncodeToString(b[:]))
	return hex.EncodeToString(m.Sum(nil))
}

func sigHeader(key string, t int64, method, path string, body []byte) string {
	return fmt.Sprintf("t=%d,caller=%s,sig=%s", t, name, sign(key, t, method, path, body))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// cors implements the DESIGN.md rules for the /_zoo endpoints, preflight included.
func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		allowed := false
		for _, a := range strings.Split(os.Getenv("ZOO_PANEL_ORIGIN"), ",") {
			if o := r.Header.Get("Origin"); o != "" && strings.TrimSpace(a) == o {
				allowed = true
				w.Header().Set("Access-Control-Allow-Origin", o)
			}
		}
		if r.Method != http.MethodOptions {
			next(w, r)
			return
		}
		if allowed {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type gateway struct {
	orders, stock string
	client        *http.Client
	probeSlot     chan struct{}
	mu            sync.Mutex
	starts        []time.Time
	now           func() time.Time
}

func newGateway(orders, stock string) *gateway {
	return &gateway{
		orders: strings.TrimRight(orders, "/"), stock: strings.TrimRight(stock, "/"),
		client: &http.Client{}, probeSlot: make(chan struct{}, 1), now: time.Now,
	}
}

// allowStart is the 10 chain starts per minute limit.
func (g *gateway) allowStart() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now, kept := g.now(), g.starts[:0]
	for _, t := range g.starts {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if g.starts = kept; len(kept) >= 10 {
		return false
	}
	g.starts = append(g.starts, now)
	return true
}

func (g *gateway) call(ctx context.Context, timeout time.Duration, method, target string, body []byte, header http.Header) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := g.client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 0, nil, fmt.Errorf("timeout after %d ms", timeout.Milliseconds())
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, data, err
}

func (g *gateway) forward(w http.ResponseWriter, r *http.Request, target string) {
	code, data, err := g.call(r.Context(), 5*time.Second, http.MethodGet, target, nil, nil)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(data)
}

func health(w http.ResponseWriter, r *http.Request) {
	build := map[string]string{"runtime": "go " + strings.TrimPrefix(runtime.Version(), "go")}
	if builtAt != "" {
		build["built_at"] = builtAt
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "stack": stack, "server": serverLabel(os.Getenv("PUBLIC_HOST")),
		"release": release(), "env": envOr("OX_ENV", "local"),
		"uptime_s": int(time.Since(startedAt).Seconds()), "started_at": startedAt.Format(time.RFC3339),
		"build": build,
	})
}

// start runs an order through the shop-order chain. The panel's chain start
// always orders ZOO-1; the shop page names its sku. Both share the rate limit.
func (g *gateway) start(fixedSKU string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c := r.PathValue("chain"); fixedSKU != "" && c != "shop-order" {
			fail(w, http.StatusNotFound, "unknown chain")
			return
		}
		var in struct{ Trace, SKU string }
		err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
		if fixedSKU != "" {
			in.SKU = fixedSKU
		}
		if err != nil || !traceRE.MatchString(in.Trace) || !skuRE.MatchString(in.SKU) {
			fail(w, http.StatusBadRequest, "bad trace or sku")
			return
		}
		if !g.allowStart() {
			fail(w, http.StatusTooManyRequests, "rate limited")
			return
		}
		body, _ := json.Marshal(map[string]string{"trace": in.Trace, "sku": in.SKU})
		code, data, err := g.call(r.Context(), 5*time.Second, http.MethodPost, g.orders+"/chain", body, jsonHdr)
		if err == nil && code != http.StatusAccepted {
			err = fmt.Errorf("orders worker answered %d: %s", code, bytes.TrimSpace(data))
		}
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"trace": in.Trace, "started": true})
	}
}

func (g *gateway) trace(w http.ResponseWriter, r *http.Request) {
	if id := r.PathValue("id"); !traceRE.MatchString(id) {
		fail(w, http.StatusBadRequest, "bad trace")
	} else {
		g.forward(w, r, g.orders+"/trace/"+id)
	}
}

func main() {
	orders, stock := os.Getenv("ORDERS_URL"), os.Getenv("STOCK_URL")
	if orders == "" || stock == "" || os.Getenv("PORT") == "" {
		log.Fatal("PORT, ORDERS_URL and STOCK_URL must be set (ox provides them)")
	}
	g := newGateway(orders, stock)
	mux := http.NewServeMux()
	mux.HandleFunc("OPTIONS /_zoo/", cors(nil))
	mux.HandleFunc("GET /_zoo/health", cors(health))
	mux.HandleFunc("GET /_zoo/probe", cors(g.probe))
	mux.HandleFunc("GET /_zoo/trace/{id}", cors(g.trace))
	mux.HandleFunc("POST /_zoo/chain/{chain}", cors(g.start("ZOO-1")))
	mux.HandleFunc("GET /api/stock", func(w http.ResponseWriter, r *http.Request) { g.forward(w, r, g.stock+"/items") })
	mux.HandleFunc("GET /api/orders", func(w http.ResponseWriter, r *http.Request) { g.forward(w, r, g.orders+"/orders") })
	mux.HandleFunc("POST /api/orders", g.start(""))
	addr := envOr("HOST", "127.0.0.1") + ":" + os.Getenv("PORT")
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}
	log.Printf("gateway on %s, orders %s, stock %s", addr, orders, stock)
	log.Fatal(srv.ListenAndServe())
}
