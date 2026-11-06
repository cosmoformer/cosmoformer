package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type LoginInput struct {
	Body struct {
		Username string `json:"username" maxLength:"64"`
		Password string `json:"password" maxLength:"1024"`
	}
}
type LoginOutput struct {
	SetCookie string `header:"Set-Cookie"`
	Body      struct {
		OK bool `json:"ok"`
	}
}
type ActorPublic struct {
	ID                string `json:"id" format:"uuid"`
	Username          string `json:"username"`
	DisplayName       string `json:"display_name"`
	InstallationAdmin bool   `json:"installation_admin"`
}
type SessionOutput struct {
	Body struct {
		Actor     ActorPublic `json:"actor"`
		CSRF      string      `json:"csrf_token"`
		ExpiresAt string      `json:"expires_at" format:"date-time"`
		store.Directory
	}
}
type ProofInput struct {
	Body struct {
		Code     string `json:"code" minLength:"43" maxLength:"43"`
		Password string `json:"password" maxLength:"1024"`
	}
}

func (a *Server) registerBrowser(api huma.API) {
	register(api, "POST", "/session/login", "login", func(ctx context.Context, in *LoginInput) (*LoginOutput, error) {
		name, _ := identity.Username(in.Body.Username)
		if name == "" {
			name = "!invalid"
		}
		subject := identity.Digest("login/" + name)
		wait, e := a.Store.Rate(ctx, subject, "check")
		if e != nil {
			return nil, fail(ctx, e)
		}
		if wait > 0 {
			return nil, huma.ErrorWithHeaders(problem(ctx, 429, "Too many attempts. Try again later."), http.Header{"Retry-After": {strconv.Itoa(wait)}})
		}
		account, e := a.Store.LoginAccount(ctx, name)
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return nil, fail(ctx, e)
		}
		verify := a.Hasher.Verify(ctx, in.Body.Password, account.PasswordPHC)
		if errors.Is(verify, identity.ErrBusy) {
			return nil, problem(ctx, 429, "Sign-in is busy. Try again shortly.")
		}
		if verify != nil || !account.Enabled {
			if _, e = a.Store.Rate(ctx, subject, "failure"); e != nil {
				return nil, fail(ctx, e)
			}
			return nil, problem(ctx, 401, "Username or password is incorrect.")
		}
		token := identity.Token()
		accepted, e := a.Store.CreateSession(ctx, account.ID, account.CredentialRevision, identity.Digest(token), identity.Digest(identity.CSRF(token)))
		if e != nil {
			return nil, fail(ctx, e)
		}
		if !accepted {
			return nil, problem(ctx, 401, "Username or password is incorrect.")
		}
		if _, e = a.Store.Rate(ctx, subject, "success"); e != nil {
			return nil, fail(ctx, e)
		}
		cookie := (&http.Cookie{Name: cookieName, Value: token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}).String()
		out := &LoginOutput{SetCookie: cookie}
		out.Body.OK = true
		return out, nil
	})
	register(api, "GET", "/session", "session", func(ctx context.Context, _ *Empty) (*SessionOutput, error) {
		out := &SessionOutput{}
		e := a.tx(ctx, "", func(tx pgx.Tx, actor store.Actor) error {
			out.Body.Actor = ActorPublic{actor.ID, actor.Username, actor.DisplayName, actor.InstallationAdmin}
			out.Body.CSRF = identity.CSRF(request(ctx).Token)
			out.Body.ExpiresAt = actor.ExpiresAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
			dir, e := store.DirectoryFor(ctx, tx, actor.ID)
			out.Body.Directory = dir
			return e
		})
		return out, e
	})
	register(api, "POST", "/session/activity", "activity", func(ctx context.Context, _ *Empty) (*OK, error) {
		e := a.tx(ctx, "", func(tx pgx.Tx, _ store.Actor) error {
			var accepted bool
			if e := tx.QueryRow(ctx, `SELECT cosmoformer.session_activity($1)`, request(ctx).Hash).Scan(&accepted); e != nil {
				return e
			}
			if !accepted {
				return store.ErrUnauthenticated
			}
			return nil
		})
		return ok(), e
	})
	register(api, "POST", "/session/logout", "logout", func(ctx context.Context, _ *Empty) (*LoginOutput, error) {
		e := a.tx(ctx, "", func(tx pgx.Tx, _ store.Actor) error {
			_, e := tx.Exec(ctx, `SELECT cosmoformer.session_logout($1)`, request(ctx).Hash)
			return e
		})
		out := &LoginOutput{SetCookie: (&http.Cookie{Name: cookieName, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1}).String()}
		out.Body.OK = true
		return out, e
	})
	for _, purpose := range []string{"setup", "reset"} {
		register(api, "POST", "/account/"+purpose, "account-"+purpose, func(ctx context.Context, in *ProofInput) (*OK, error) {
			if !identity.ValidToken(in.Body.Code) {
				return nil, problem(ctx, 422, "The code is invalid or expired.")
			}
			phc, e := a.Hasher.Hash(ctx, in.Body.Password)
			if errors.Is(e, identity.ErrBusy) {
				return nil, problem(ctx, 429, "Try again shortly.")
			}
			if e != nil {
				return nil, problem(ctx, 422, e.Error())
			}
			accepted, e := a.Store.ConsumeProof(ctx, identity.Digest(in.Body.Code), purpose, phc)
			if e != nil {
				return nil, fail(ctx, e)
			}
			if !accepted {
				return nil, problem(ctx, 422, "The code is invalid or expired.")
			}
			return ok(), nil
		})
	}
}
