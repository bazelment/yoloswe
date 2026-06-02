package hub

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errUnknownMachine = errors.New("unknown or disconnected machine")

// Authenticator implements simple shared-secret browser auth: POST /login with
// the secret mints a random session cookie held in memory. This mirrors
// tmux-mobile's network-boundary + token trust model (intended to run behind
// Tailscale/TLS), kept deliberately minimal and pluggable.
type Authenticator struct {
	sessions   map[string]time.Time // token -> expiry
	secret     string
	cookieName string
	ttl        time.Duration
	mu         sync.Mutex
}

// NewAuthenticator creates a shared-secret browser Authenticator.
func NewAuthenticator(secret string) *Authenticator {
	return &Authenticator{
		secret:     secret,
		cookieName: "bramble_hub_session",
		sessions:   make(map[string]time.Time),
		ttl:        24 * time.Hour,
	}
}

// handleLogin accepts the shared secret (form field or query "secret") and sets
// a session cookie. A GET renders a minimal login form.
func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(loginHTML))
		return
	}
	secret := r.FormValue("secret")
	if subtle.ConstantTimeCompare([]byte(secret), []byte(a.secret)) != 1 {
		http.Error(w, "invalid secret", http.StatusUnauthorized)
		return
	}
	tok := randomToken()
	a.mu.Lock()
	a.sessions[tok] = nowUTC().Add(a.ttl)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		// Secure when the login arrived over TLS (the intended prod posture behind
		// TLS/Tailscale); left off for plain-HTTP local dev so the cookie is still
		// sent back. r.TLS is nil behind a terminating proxy, so also honor
		// X-Forwarded-Proto.
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// valid reports whether the request carries a live session cookie.
func (a *Authenticator) valid(r *http.Request) bool {
	c, err := r.Cookie(a.cookieName)
	if err != nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[c.Value]
	if !ok {
		return false
	}
	if nowUTC().After(exp) {
		delete(a.sessions, c.Value)
		return false
	}
	return true
}

// requireAuth wraps a JSON API handler, returning 401 when unauthenticated.
func (h *Hub) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.auth.valid(r) {
			writeJSON(w, http.StatusUnauthorized, errBody(errors.New("unauthenticated")))
			return
		}
		next(w, r)
	}
}

// requireAuthPage wraps a page handler, redirecting to /login when unauthed.
func (h *Hub) requireAuthPage(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.auth.valid(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// nowUTC is overridable in tests; production uses the wall clock.
var nowUTC = func() time.Time { return time.Now().UTC() }

const loginHTML = `<!doctype html><html><head><meta charset=utf-8>
<meta name=viewport content="width=device-width,initial-scale=1">
<title>bramble hub — login</title>
<style>body{font-family:system-ui;background:#1a1a1a;color:#eee;display:grid;place-items:center;height:100vh;margin:0}
form{background:#262626;padding:2rem;border-radius:12px;display:flex;flex-direction:column;gap:1rem;min-width:260px}
input,button{padding:.6rem;border-radius:8px;border:1px solid #444;background:#1a1a1a;color:#eee;font-size:1rem}
button{background:#3b82f6;border:none;cursor:pointer}</style></head>
<body><form method=post action=/login>
<h2 style=margin:0>bramble hub</h2>
<input type=password name=secret placeholder="access secret" autofocus>
<button type=submit>Enter</button></form></body></html>`
