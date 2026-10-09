package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testKey = "zoo-test-key-0123456789abcdef"

func TestSigningVectors(t *testing.T) {
	if got := sign(testKey, 1760000000, "post", "/api/items?x=1", []byte(`{"sku":"ZOO-1"}`)); got != "50c22839fe6a06cb51a9fd25167d9e457eb0b5ee63ce696f4c5428a6b9271da1" {
		t.Fatalf("POST sig %s", got)
	}
	if got := sign(testKey, 1760000000, "GET", "/_zoo/verify", nil); got != "9a404bebaa32497c5ed39ef8990e8466428f8023d6aa9f5acc94f16fb7670ecb" {
		t.Fatalf("GET sig %s", got)
	}
	if fp(testKey) != "915a" {
		t.Fatalf("fp %s", fp(testKey))
	}
	if h := sigHeader(testKey, 1760000000, "GET", "/_zoo/verify", nil); !strings.HasPrefix(h, "t=1760000000,caller=mesh-shop,sig=9a404b") {
		t.Fatalf("header %s", h)
	}
}

func TestServerLabelAndTrace(t *testing.T) {
	if serverLabel("mesh-shop.s3.zoo.sorv.dev") != "s3" || serverLabel("") != "local" || serverLabel("x.sx.dev") != "local" {
		t.Fatal("server label")
	}
	for _, bad := range []string{"", "abc", "3F9C2A10-0000-4000-8000-000000000000", "3f9c2a10-0000-4000-8000-0000000000000"} {
		if traceRE.MatchString(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
	g := newGateway("http://127.0.0.1:1", "http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/_zoo/trace/nope", nil)
	req.SetPathValue("id", "nope")
	g.trace(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad trace gave %d", rec.Code)
	}
}

func TestCORS(t *testing.T) {
	t.Setenv("ZOO_PANEL_ORIGIN", "https://zoo-control.s1.zoo.sorv.dev, http://localhost:5173")
	h := cors(health)
	for _, tc := range []struct {
		method, origin, allow string
		code                  int
	}{
		{"GET", "http://localhost:5173", "http://localhost:5173", 200},
		{"GET", "https://evil.example", "", 200},
		{"OPTIONS", "https://zoo-control.s1.zoo.sorv.dev", "https://zoo-control.s1.zoo.sorv.dev", 204},
		{"OPTIONS", "https://evil.example", "", 204},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, "/_zoo/health", nil)
		req.Header.Set("Origin", tc.origin)
		h(rec, req)
		hd := rec.Header()
		if rec.Code != tc.code || hd.Get("Access-Control-Allow-Origin") != tc.allow || hd.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("%+v: code %d allow %q", tc, rec.Code, hd.Get("Access-Control-Allow-Origin"))
		}
		if tc.method == "OPTIONS" && tc.allow != "" && (hd.Get("Access-Control-Allow-Methods") != "GET, POST, OPTIONS" || hd.Get("Access-Control-Max-Age") != "600") {
			t.Fatal("preflight headers")
		}
		if tc.allow == "" && hd.Get("Access-Control-Allow-Methods") != "" {
			t.Fatal("headers for unlisted origin")
		}
	}
}

func TestRateLimit(t *testing.T) {
	g := newGateway("", "")
	now := time.Unix(1760000000, 0)
	g.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		if !g.allowStart() {
			t.Fatalf("start %d refused", i)
		}
	}
	if g.allowStart() {
		t.Fatal("11th start allowed")
	}
	now = now.Add(61 * time.Second)
	if !g.allowStart() {
		t.Fatal("window did not reset")
	}
}

func fakePeer(t *testing.T, peerName, keyFP string, publicURL *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_zoo/verify" || !strings.Contains(r.Header.Get("X-Zoo-Signature"), "caller=mesh-shop,sig=") {
			writeJSON(w, 401, map[string]any{"ok": false, "error": "missing signature"})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "name": peerName, "public_url": *publicURL, "key_fp": keyFP, "verified_by": "INTERNAL_TOKEN"})
	}))
}

func TestVerifyPeer(t *testing.T) {
	var public string
	srv := fakePeer(t, "catalog-api", "915a", &public)
	defer srv.Close()
	public = srv.URL
	t.Setenv("CATALOG_URL", srv.URL+"/")
	t.Setenv("INTERNAL_TOKEN", testKey)
	g := newGateway("", "")
	p := peers[0]
	if c := g.verifyPeer(t.Context(), "mesh-shop@local", p, time.Now()); !c.OK {
		t.Fatalf("match failed: %+v", c)
	}
	t.Setenv("INTERNAL_TOKEN", "another-key")
	if c := g.verifyPeer(t.Context(), "mesh-shop@local", p, time.Now()); c.OK || !strings.Contains(c.Error, "key_fp mismatch") {
		t.Fatalf("fp mismatch not caught: %+v", c)
	}
	t.Setenv("INTERNAL_TOKEN", testKey)
	public = "https://elsewhere.example"
	if c := g.verifyPeer(t.Context(), "mesh-shop@local", p, time.Now()); c.OK || !strings.Contains(c.Error, "public_url") {
		t.Fatalf("url mismatch not caught: %+v", c)
	}
	t.Setenv("INTERNAL_TOKEN", "")
	if c := g.verifyPeer(t.Context(), "mesh-shop@local", p, time.Now()); c.Error != "INTERNAL_TOKEN is not set" {
		t.Fatalf("missing key: %+v", c)
	}
}

func TestProbeAggregates(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/checks" {
			writeJSON(w, 200, []check{{ID: "postgres", OK: true, Env: []string{"DATABASE_URL"}}, {ID: "redis", OK: true}, {ID: "fulfiller", OK: true}})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "detail": "fine"})
	}))
	defer worker.Close()
	t.Setenv("CATALOG_URL", "")
	t.Setenv("INTERNAL_TOKEN", "secret-value")
	g := newGateway(worker.URL, worker.URL)
	rec := httptest.NewRecorder()
	g.probe(rec, httptest.NewRequest("GET", "/_zoo/probe", nil))
	var out struct {
		OK     bool
		Checks []check
		Vars   []variable
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK || len(out.Checks) != 7 || !out.Checks[0].OK || out.Checks[5].Error != "CATALOG_URL is not set" {
		t.Fatalf("probe %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-value") || out.Vars[0].FP != fp("secret-value") || !out.Vars[2].Missing {
		t.Fatalf("vars %+v", out.Vars)
	}
}

func TestStartValidates(t *testing.T) {
	g := newGateway("http://127.0.0.1:1", "")
	for _, tc := range []struct {
		h    http.HandlerFunc
		path string
		body string
		code int
	}{
		{g.start("ZOO-1"), "other", `{"trace":"3f9c2a10-1b2c-4d5e-8f90-a1b2c3d4e5f6"}`, 404},
		{g.start("ZOO-1"), "shop-order", `{"trace":"nope"}`, 400},
		{g.start(""), "", `{"trace":"3f9c2a10-1b2c-4d5e-8f90-a1b2c3d4e5f6","sku":"../x"}`, 400},
		{g.start("ZOO-1"), "shop-order", `{"trace":"3f9c2a10-1b2c-4d5e-8f90-a1b2c3d4e5f6"}`, 502},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
		req.SetPathValue("chain", tc.path)
		tc.h(rec, req)
		if rec.Code != tc.code {
			t.Fatalf("%s %s: %d, want %d", tc.path, tc.body, rec.Code, tc.code)
		}
	}
}
