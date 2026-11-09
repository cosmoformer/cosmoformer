package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testAuthOrigin = "https://localhost:5173"

func authRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", testAuthOrigin)
	r.Header.Set("X-Cosmoformer-Request", "browser")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

func TestPublicAuthUnsupportedMethodsDoNotSpendBudget(t *testing.T) {
	for _, path := range []string{"/api/v1/session/login", "/api/v1/account/setup", "/api/v1/account/reset"} {
		t.Run(path, func(t *testing.T) {
			h := (&Server{Origin: testAuthOrigin}).middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
			for range 12 {
				for _, method := range []string{"GET", "HEAD", "OPTIONS", "PUT"} {
					w := httptest.NewRecorder()
					h.ServeHTTP(w, authRequest(method, path))
					if w.Code != 405 || w.Header().Get("Allow") != "POST" {
						t.Fatalf("unsupported method: %s %d", method, w.Code)
					}
				}
			}
			for i := range 11 {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, authRequest("POST", path))
				want := 204
				if i == 10 {
					want = 429
				}
				if w.Code != want {
					t.Fatalf("POST %d: got %d want %d", i+1, w.Code, want)
				}
			}
		})
	}
}

func TestPublicAuthUnixClientBudgetsAndInvalidMetadata(t *testing.T) {
	dir, err := os.MkdirTemp("", "cf-auth-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	h := (&Server{Origin: testAuthOrigin}).middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	server := &http.Server{Handler: h, ConnContext: ConnectionContext, ReadHeaderTimeout: time.Second}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	send := func(values []string, want int) {
		t.Helper()
		r := authRequest("POST", "/api/v1/session/login")
		r.RequestURI = ""
		r.URL.Scheme, r.URL.Host = "http", "private-api"
		for _, value := range values {
			r.Header.Add(clientIPHeader, value)
		}
		r.Header.Set("X-Forwarded-For", "203.0.113.99")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("metadata %q: got %d want %d", values, response.StatusCode, want)
		}
	}
	for range 10 {
		send([]string{"192.0.2.1"}, 204)
	}
	send([]string{"192.0.2.1"}, 429)
	send([]string{"::ffff:192.0.2.1"}, 429)
	send([]string{"192.0.2.2"}, 204)
	send(nil, 204) // owner-only direct IPC has separate admission
	for _, values := range [][]string{{""}, {"localhost"}, {"192.0.2.2:9"}, {"192.0.2.2, 192.0.2.3"}, {"fe80::1%lo"}, {"::ffff:192.0.2.2%lo"}, {"0.0.0.0"}, {"::ffff:0.0.0.0"}, {"::"}, {"192.0.2.2", "192.0.2.3"}} {
		send(values, 400)
	}
	for range 9 {
		send([]string{"192.0.2.2"}, 204)
	}
	send([]string{"192.0.2.2"}, 429)
	send([]string{"2001:db8::1"}, 204)
}

func TestPublicAuthTCPDisregardsForwardingClaims(t *testing.T) {
	h := (&Server{Origin: testAuthOrigin}).middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	server := httptest.NewUnstartedServer(h)
	server.Config.ConnContext = ConnectionContext
	server.Start()
	t.Cleanup(server.Close)
	for i := range 11 {
		r := authRequest("POST", "/api/v1/session/login")
		r.RequestURI = ""
		base := httptest.NewRequest("POST", server.URL, nil)
		r.URL.Scheme, r.URL.Host = base.URL.Scheme, base.URL.Host
		r.Header.Set(clientIPHeader, "192.0.2."+strings.Repeat("1", i+1))
		r.Header.Set("X-Forwarded-For", r.Header.Get(clientIPHeader))
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := 204
		if i == 10 {
			want = 429
		}
		if response.StatusCode != want {
			t.Fatalf("spoofed identity %d: got %d want %d", i, response.StatusCode, want)
		}
	}
}

func TestAuthenticationClientCanonicalization(t *testing.T) {
	for _, tc := range []struct{ peer, want string }{{"[::ffff:192.0.2.1]:1", "peer:192.0.2.1"}, {"[2001:db8:0::1]:9", "peer:2001:db8::1"}, {"@", ""}, {"localhost:1", ""}, {"[fe80::1%lo]:1", ""}, {"[::ffff:192.0.2.1%lo]:1", ""}, {"0.0.0.0:1", ""}, {"[::ffff:0.0.0.0]:1", ""}} {
		r := authRequest("POST", "/api/v1/session/login")
		r.RemoteAddr = tc.peer
		got, ok := authenticationClient(r)
		if got != tc.want || ok != (tc.want != "") {
			t.Fatalf("peer %q: %q %t", tc.peer, got, ok)
		}
	}
	h := (&Server{Origin: testAuthOrigin}).middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid transport reached handler") }))
	r := authRequest("POST", "/api/v1/session/login")
	r.RemoteAddr = "@"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("invalid peer was admitted", w.Code)
	}
}
