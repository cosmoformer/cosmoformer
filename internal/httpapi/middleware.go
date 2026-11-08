package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
)

const cookieName = "__Host-cosmoformer_session"

type requestKey struct{}
type requestData struct {
	Hash           []byte
	Token, ID, Key string
	Digest         []byte
}

func request(ctx context.Context) requestData {
	d, _ := ctx.Value(requestKey{}).(requestData)
	return d
}

type Problem struct {
	Status      int    `json:"status"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	RequestID   string `json:"request_id"`
	ExpectedSeq int64  `json:"expected_seq,omitempty"`
}

func (p *Problem) Error() string  { return p.Message }
func (p *Problem) GetStatus() int { return p.Status }
func problem(ctx context.Context, status int, message string) *Problem {
	codes := map[int]string{400: "INVALID_REQUEST", 401: "SIGN_IN_REQUIRED", 403: "FORBIDDEN", 404: "NOT_FOUND", 409: "CONFLICT", 422: "INVALID_INPUT", 429: "TRY_AGAIN", 503: "UNAVAILABLE"}
	return &Problem{Status: status, Code: codes[status], Message: message, RequestID: request(ctx).ID}
}
func init() {
	huma.NewError = func(status int, message string, errs ...error) huma.StatusError {
		if len(errs) > 0 {
			message = "Check the submitted fields and try again."
		}
		return problem(context.Background(), status, message)
	}

	huma.NewErrorWithContext = func(ctx huma.Context, status int, message string, errs ...error) huma.StatusError {
		// Huma validation details can contain submitted credential values. Never echo
		// input values, parser errors or database errors in the public envelope.
		if len(errs) > 0 {
			message = "Check the submitted fields and try again."
		}
		return problem(ctx.Context(), status, message)
	}
}
func writeProblem(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(problem(r.Context(), status, message))
}

type bucket struct {
	tokens float64
	at     time.Time
}
type limiter struct {
	mu  sync.Mutex
	ips map[string]bucket
}

func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.ips) >= 10000 {
		for k, b := range l.ips {
			if now.Sub(b.at) > 15*time.Minute {
				delete(l.ips, k)
			}
		}
	}
	b, ok := l.ips[ip]
	if !ok {
		if len(l.ips) >= 10000 {
			return false
		}
		b = bucket{10, now}
	}
	b.tokens = min(10, b.tokens+now.Sub(b.at).Seconds()*5/60)
	b.at = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	l.ips[ip] = b
	return allowed
}
func (a *Server) middleware(next http.Handler) http.Handler {
	l := &limiter{ips: map[string]bucket{}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := requestData{ID: identity.Token()[:16]}
		if c, e := r.Cookie(cookieName); e == nil && identity.ValidToken(c.Value) {
			d.Token = c.Value
			d.Hash = identity.Digest(c.Value)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestKey{}, d))
		w.Header().Set("X-Request-ID", d.ID)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		limit := int64(64 * 1024)
		if strings.HasPrefix(r.URL.Path, "/worker/") || strings.HasPrefix(r.URL.Path, "/webhooks/") {
			limit = 1048576
		}
		if r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/worker/v1/attempts/") && strings.Contains(r.URL.Path, "/artifacts/") {
			limit = 67108865
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			public := r.URL.Path == "/api/v1/session/login" || r.URL.Path == "/api/v1/account/setup" || r.URL.Path == "/api/v1/account/reset"
			mutation := r.Method != "GET" && r.Method != "HEAD"
			if mutation {
				media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if e != nil || media != "application/json" || r.Header.Get("Origin") != a.Origin || r.Header.Get("X-Cosmoformer-Request") != "browser" || (r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin") {
					writeProblem(w, r, 403, "This request must come from Cosmoformer.")
					return
				}
			}
			if public {
				ip, _, _ := net.SplitHostPort(r.RemoteAddr)
				// Forwarding headers are deliberately ignored. Only the local/private
				// reverse proxy connects here; no arbitrary trusted-proxy configuration.
				if !l.allow(ip) {
					w.Header().Set("Retry-After", "60")
					writeProblem(w, r, 429, "Too many attempts. Try again shortly.")
					return
				}
			} else {
				if len(d.Hash) == 0 {
					clearCookie(w)
					writeProblem(w, r, 401, "Sign in to continue.")
					return
				}
				actor, e := a.Store.Session(r.Context(), d.Hash)
				if e != nil {
					if e == store.ErrUnauthenticated {
						clearCookie(w)
						writeProblem(w, r, 401, "Sign in to continue.")
					} else {
						writeProblem(w, r, 503, "The service is temporarily unavailable.")
					}
					return
				}
				if mutation && !actor.CSRFMatches(identity.Digest(r.Header.Get("X-CSRF-Token"))) {
					writeProblem(w, r, 403, "Refresh the page and try again.")
					return
				}
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != "GET" && r.Method != "HEAD" && !strings.Contains(r.URL.Path, "/session/") && !strings.Contains(r.URL.Path, "/account/") {
			d.Key = r.Header.Get("Idempotency-Key")
			if !regexp.MustCompile(`^[a-zA-Z0-9_-]{16,128}$`).MatchString(d.Key) {
				writeProblem(w, r, 422, "A valid Idempotency-Key is required.")
				return
			}
			body, e := io.ReadAll(r.Body)
			if e != nil {
				writeProblem(w, r, 422, "Request is too large or invalid.")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			// Secret writes retain only their one-use change identity, never a fast
			// verifier of the submitted plaintext. Their receipts are metadata only.
			secretWrite := strings.HasSuffix(r.URL.Path, "/secrets") || strings.Contains(r.URL.Path, "/secrets/") || strings.HasSuffix(r.URL.Path, "/source-credentials") || strings.HasSuffix(r.URL.Path, "/credentials")
			if secretWrite {
				d.Digest = identity.Digest(r.Method + "\x00" + r.URL.Path + "\x00" + d.Key)
			} else {
				d.Digest = identity.Digest(r.Method + "\x00" + r.URL.Path + "\x00" + string(body))
			}
			r = r.WithContext(context.WithValue(r.Context(), requestKey{}, d))
		}
		next.ServeHTTP(w, r)
	})
}
func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}
