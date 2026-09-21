package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testAddress = "https://ai-hub.tail0000.ts.net"

// upstream records what the hub would see and answers 200 with a body.
func upstream(t *testing.T) (*httptest.Server, *http.Request) {
	t.Helper()
	var seen http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r
		seen.Header = r.Header.Clone()
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hub says hello"))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func handlerFor(t *testing.T, upstreamURL string) http.Handler {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	return proxyHandler(config{upstream: u, marker: "marker-token-0123456789"}, testAddress)
}

func TestForwardsWithMarkerAndLoopbackHost(t *testing.T) {
	hub, seen := upstream(t)
	h := handlerFor(t, hub.URL)
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Host = "ai-hub.tail0000.ts.net"
	req.Header.Set("X-AI-Hub-Phone", "forged")
	req.Header.Set("X-AI-Hub-Run", "forged-run")
	req.Header.Set("Cookie", "ai-hub-phone=abc")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "hub says hello" {
		t.Fatalf("expected the hub's answer, got %d %q", rec.Code, rec.Body.String())
	}
	if got := seen.Header.Get("X-AI-Hub-Phone"); got != "marker-token-0123456789" {
		t.Fatalf("marker not set or forged one kept: %q", got)
	}
	if seen.Header.Get("X-AI-Hub-Run") != "" {
		t.Fatal("the bridge header from the phone reached the hub")
	}
	wantHost := strings.TrimPrefix(hub.URL, "http://")
	if seen.Host != wantHost {
		t.Fatalf("host not rewritten to the loopback: %q", seen.Host)
	}
	if seen.Header.Get("Cookie") != "ai-hub-phone=abc" || seen.Header.Get("Sec-Fetch-Site") != "same-origin" {
		t.Fatal("cookie or fetch metadata did not travel")
	}
	if seen.Header.Get("X-Forwarded-Proto") != "https" || seen.Header.Get("X-Forwarded-Host") != "ai-hub.tail0000.ts.net" {
		t.Fatal("forwarded headers missing")
	}
}

func TestOwnOriginIsRewrittenToLoopback(t *testing.T) {
	hub, seen := upstream(t)
	h := handlerFor(t, hub.URL)
	req := httptest.NewRequest("POST", "/api/phone/login", strings.NewReader(`{"password":"x"}`))
	req.Header.Set("Origin", testAddress)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := seen.Header.Get("Origin"); got != "http://"+strings.TrimPrefix(hub.URL, "http://") {
		t.Fatalf("origin not rewritten to the hub's own: %q", got)
	}
}

func TestForeignOriginIsRefusedBeforeTheHub(t *testing.T) {
	hub, seen := upstream(t)
	h := handlerFor(t, hub.URL)
	for _, origin := range []string{"https://evil.example", "http://ai-hub.tail0000.ts.net", "https://ai-hub.tail0000.ts.net.evil.example", "null"} {
		req := httptest.NewRequest("POST", "/api/projects", strings.NewReader(`{}`))
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("origin %q: expected 403, got %d", origin, rec.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "Untrusted origin" {
			t.Fatalf("origin %q: unexpected body %q", origin, rec.Body.String())
		}
		if seen.Method != "" {
			t.Fatalf("origin %q: the hub was called", origin)
		}
	}
}

func TestHubDownAnswers502(t *testing.T) {
	h := handlerFor(t, "http://127.0.0.1:1")
	req := httptest.NewRequest("GET", "/api/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "phone-hub-unreachable" {
		t.Fatalf("unexpected body %q", rec.Body.String())
	}
}
