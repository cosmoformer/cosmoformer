//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
)

// Seed capacity directly: this checks conditional admission/accounting, not
// whether an attacker can attain capacity through a deployed ingress.
func TestLoginThrottleCapacity(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	const password = "Synthetic capacity password 2026"
	phc, err := identity.NewHasher().Hash(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Owner.Exec(ctx, `INSERT INTO cosmoformer.accounts(username,display_name,password_phc) VALUES ('untracked','Synthetic untracked account',$1),('tracked','Synthetic tracked account',$1)`, phc); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Owner.Exec(ctx, `INSERT INTO cosmoformer.auth_throttles(subject,failures,window_at) SELECT sha256(convert_to('synthetic-capacity/'||n::text,'UTF8')),0,clock_timestamp() FROM generate_series(1,9999) n`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Owner.Exec(ctx, `INSERT INTO cosmoformer.auth_throttles(subject,failures,window_at) VALUES($1,0,clock_timestamp())`, identity.Digest("login/tracked")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tracked", "untracked"} {
		t.Run(name, func(t *testing.T) {
			handler, _ := New(db.App, testAuthOrigin)
			server := httptest.NewServer(handler)
			defer server.Close()
			client := &http.Client{Timeout: 15 * time.Second}
			statuses := make([]int, 0, 6)
			for range 6 { // Below the unchanged single-peer burst budget.
				body, _ := json.Marshal(map[string]string{"username": name, "password": "Synthetic incorrect password"})
				r, _ := http.NewRequest("POST", server.URL+"/api/v1/session/login", bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Origin", testAuthOrigin)
				r.Header.Set("X-Cosmoformer-Request", "browser")
				response, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				statuses = append(statuses, response.StatusCode)
				if response.StatusCode == 429 {
					wait, err := strconv.Atoi(response.Header.Get("Retry-After"))
					if err != nil || wait <= 0 {
						t.Fatal("missing positive retry wait")
					}
				}
			}
			var count, rows, failures, sessions int
			if err := db.Owner.QueryRow(ctx, `SELECT count(*),(SELECT count(*) FROM cosmoformer.auth_throttles WHERE subject=$1),(SELECT coalesce(max(failures),0) FROM cosmoformer.auth_throttles WHERE subject=$1),(SELECT count(*) FROM cosmoformer.sessions) FROM cosmoformer.auth_throttles`, identity.Digest("login/"+name)).Scan(&count, &rows, &failures, &sessions); err != nil {
				t.Fatal(err)
			}
			t.Logf("synthetic %s: statuses=%v table_rows=%d subject_rows=%d failures=%d sessions=%d", name, statuses, count, rows, failures, sessions)
			for i, status := range statuses {
				want := 429
				if name == "tracked" && i < 5 {
					want = 401
				}
				if status != want {
					t.Errorf("request %d: got %d, want %d", i+1, status, want)
				}
			}
			if count != 10000 || sessions != 0 || name == "tracked" && (rows != 1 || failures != 5) || name == "untracked" && rows != 0 {
				t.Fatal("capacity or accounting invariant changed")
			}
		})
	}
	// Capacity recovery must admit a real correct login and release its zero-
	// failure reservation, rather than leaving successful subjects in the table.
	if _, err = db.Owner.Exec(ctx, `DELETE FROM cosmoformer.auth_throttles WHERE subject=sha256(convert_to('synthetic-capacity/1','UTF8'))`); err != nil {
		t.Fatal(err)
	}
	handler, _ := New(db.App, testAuthOrigin)
	r := authRequest("POST", "/api/v1/session/login")
	body, _ := json.Marshal(map[string]string{"username": "untracked", "password": password})
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var count, rows, sessions int
	if err = db.Owner.QueryRow(ctx, `SELECT count(*),(SELECT count(*) FROM cosmoformer.auth_throttles WHERE subject=$1),(SELECT count(*) FROM cosmoformer.sessions) FROM cosmoformer.auth_throttles`, identity.Digest("login/untracked")).Scan(&count, &rows, &sessions); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || count != 9999 || rows != 0 || sessions != 1 || len(w.Result().Cookies()) != 1 {
		t.Fatal("correct login did not release its reservation", w.Code, count, rows, sessions)
	}
}

func TestLoginFailureHonorsCapacityWait(t *testing.T) {
	db := testsupport.NewDatabase(t)
	// A deterministic database-contract fixture models capacity becoming
	// unavailable after the initial check. The real handler/hasher still run.
	_, err := db.Owner.Exec(context.Background(), `CREATE OR REPLACE FUNCTION cosmoformer.auth_rate(subject_hash bytea,outcome text) RETURNS integer LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$ SELECT CASE WHEN outcome='failure' THEN 60 ELSE 0 END $$`)
	if err != nil {
		t.Fatal(err)
	}
	handler, _ := New(db.App, testAuthOrigin)
	r := authRequest("POST", "/api/v1/session/login")
	body := []byte(`{"username":"synthetic","password":"Synthetic incorrect password"}`)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("failure-accounting capacity wait was ignored", w.Code, w.Header().Get("Retry-After"))
	}
}
