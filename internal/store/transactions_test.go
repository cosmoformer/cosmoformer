package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestClassifyDatabaseFailures(t *testing.T) {
	for _, tc := range []struct{ err, want error }{
		{nil, nil}, {fmt.Errorf("wrapped: %w", pgx.ErrNoRows), ErrNotFound},
		{&pgconn.PgError{Code: "42501"}, ErrForbidden},
		{&pgconn.PgError{Code: "23505"}, ErrConflict}, {&pgconn.PgError{Code: "23503"}, ErrConflict},
		{&pgconn.PgError{Code: "23514"}, ErrConflict}, {&pgconn.PgError{Code: "22P02"}, ErrConflict},
	} {
		if got := classify(tc.err); got != tc.want {
			t.Errorf("%v: got %v want %v", tc.err, got, tc.want)
		}
	}
	unknown := errors.New("connection lost")
	if classify(unknown) != unknown {
		t.Fatal("unexpected failure hidden")
	}
}

type jsonRow struct {
	data []byte
	err  error
}

func (r jsonRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*[]byte) = r.data
	return nil
}
func (r jsonRow) QueryRow(context.Context, string, ...any) pgx.Row { return r }

func TestJSONMissingAndMalformedResults(t *testing.T) {
	for _, row := range []jsonRow{{}, {data: []byte("null")}, {err: pgx.ErrNoRows}} {
		if _, err := JSON[Account](context.Background(), row, "fixture"); err != ErrNotFound {
			t.Fatal("missing result", err)
		}
	}
	if _, err := JSON[Account](context.Background(), jsonRow{data: []byte(`{"id":`)}, "fixture"); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	got, err := JSON[Account](context.Background(), jsonRow{data: []byte(`{"id":"fixture","enabled":true}`)}, "fixture")
	if err != nil || got.ID != "fixture" || !got.Enabled {
		t.Fatal(got, err)
	}
	var account Account
	if err := DecodeJSON([]byte(`{"username":"engineer"}`), &account); err != nil || account.Username != "engineer" {
		t.Fatal(account, err)
	}
	if err := DecodeJSON([]byte("invalid"), &account); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestCSRFVerifier(t *testing.T) {
	hash := make([]byte, 32)
	actor := Actor{CSRFVerifier: hex.EncodeToString(hash)}
	if !actor.CSRFMatches(hash) {
		t.Fatal("matching verifier rejected")
	}
	hash[0] = 1
	if actor.CSRFMatches(hash) || actor.CSRFMatches(nil) {
		t.Fatal("wrong verifier accepted")
	}
	actor.CSRFVerifier = "invalid"
	if actor.CSRFMatches(hash) {
		t.Fatal("malformed verifier accepted")
	}
}
