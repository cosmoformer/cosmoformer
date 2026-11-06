package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBrowserGuardsAndContractWithoutDatabase(t *testing.T) {
	h, api := New(nil, "https://localhost:5173")
	for _, origin := range []string{"", "https://attacker.example"} {
		r := httptest.NewRequest("POST", "/api/v1/session/login", strings.NewReader(`{"username":"test","password":"never-echo-this"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Cosmoformer-Request", "browser")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 || strings.Contains(w.Body.String(), "never-echo") {
			t.Fatal("unsafe pre-auth origin handling", w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/api/v1/session", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("anonymous bootstrap", w.Code)
	}
	if api.OpenAPI().Paths["/api/v1/session/login"].Post == nil || api.OpenAPI().Paths["/worker/v1/poll"] != nil {
		t.Fatal("contract contains missing or unimplemented operations")
	}
}

func TestMutationGuardsFailBeforeHandler(t *testing.T) {
	a := &Server{Origin: "https://localhost:5173"}
	for _, path := range []string{"/api/v1/session/login", "/api/v1/account/setup", "/api/v1/account/reset"} {
		for _, field := range []string{"Content-Type", "Origin", "X-Cosmoformer-Request", "Sec-Fetch-Site"} {
			t.Run(path+field, func(t *testing.T) {
				called := false
				h := a.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
				r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", a.Origin)
				r.Header.Set("X-Cosmoformer-Request", "browser")
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				r.Header.Set(field, "invalid")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 403 || called {
					t.Fatalf("guard bypass: %d", w.Code)
				}
			})
		}
	}
	for _, metadata := range []string{"", "same-origin"} {
		called := false
		h := a.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		r := httptest.NewRequest("POST", "/api/v1/session/login", nil)
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		r.Header.Set("Origin", a.Origin)
		r.Header.Set("X-Cosmoformer-Request", "browser")
		r.Header.Set("Sec-Fetch-Site", metadata)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !called {
			t.Fatalf("valid browser request rejected: %d", w.Code)
		}
	}
}

func TestAnonymousCookiesAndHealth(t *testing.T) {
	h, _ := New(nil, "https://localhost:5173")
	for _, token := range []string{"", "invalid"} {
		r := httptest.NewRequest("GET", "/api/v1/session", nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		cookies := w.Result().Cookies()
		if w.Code != 401 || len(cookies) != 1 || cookies[0].MaxAge != -1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
			t.Fatal("unsafe anonymous response")
		}
		var p Problem
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || p.RequestID == "" || p.RequestID != w.Header().Get("X-Request-ID") || p.Code != "SIGN_IN_REQUIRED" {
			t.Fatal("missing request correlation")
		}
	}
	for _, tc := range []struct {
		path   string
		status int
	}{{"/health/live", 200}, {"/health/ready", 503}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status {
			t.Fatal(tc.path, w.Code)
		}
	}
}

func TestLimiterCapacityAndExpiry(t *testing.T) {
	l := &limiter{ips: map[string]bucket{}}
	for range 10 {
		if !l.allow("peer") {
			t.Fatal("initial burst rejected")
		}
	}
	if l.allow("peer") {
		t.Fatal("burst overflow accepted")
	}
	l.ips["peer"] = bucket{0, time.Now().Add(-time.Minute)}
	if !l.allow("peer") {
		t.Fatal("tokens never refill")
	}
	for i := range 10000 {
		l.ips[fmt.Sprint(i)] = bucket{0, time.Now()}
	}
	if l.allow("new") {
		t.Fatal("unbounded peers")
	}
	for k := range l.ips {
		l.ips[k] = bucket{0, time.Now().Add(-16 * time.Minute)}
	}
	if !l.allow("new") {
		t.Fatal("stale peers never pruned")
	}
}

func TestStoreErrorsAreSanitized(t *testing.T) {
	ctx := context.WithValue(context.Background(), requestKey{}, requestData{ID: "request-fixture"})
	for _, tc := range []struct {
		err    error
		status int
	}{
		{store.ErrUnauthenticated, 401}, {store.ErrForbidden, 403}, {store.ErrNotFound, 404}, {store.ErrConflict, 409}, {errors.New("private database details"), 503},
	} {
		err := fail(ctx, fmt.Errorf("wrapped: %w", tc.err))
		p, ok := err.(*Problem)
		if !ok || p.Status != tc.status || p.GetStatus() != tc.status || p.RequestID != "request-fixture" || strings.Contains(p.Error(), "private") {
			t.Fatal("unsafe error", err)
		}
	}
}
