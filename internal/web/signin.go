package web

// Sign-in: who may talk to aimd at all.
//
// Every request but the static files and the sign-in link itself needs a
// credential (requireSignIn):
//
//   - a program sends aimd's token in X-AIM-Token (the mod, `aim`);
//   - a browser sends the session cookie, which only GET /app/sign-in sets,
//     in exchange for a one-time code. A code comes from POST /api/sign-in,
//     which needs the token: `aim open` and the mod's /aim ask for one and
//     open the link, so only someone who can read ~/.ai-manager/token can
//     sign a browser in.
//
// Without this a dashboard page, and the CSRF token it embeds, was there
// for any local process to fetch: another account on the Mac, a browser
// extension, a sandboxed process with loopback access. That token then
// passed every write the dashboard can make.
//
// The cookie is a signed nonce and issue time (signedToken), keyed from
// aimd's token under its own label, so it survives a restart of aimd, a
// CSRF token is no session and a new token file signs every browser out.
// It is HttpOnly, SameSite=Lax and named for the port: browsers don't
// separate cookies by port, so a development aimd on another port keeps its
// own. What is left open: a browser sends the cookie to any server on
// 127.0.0.1, whatever its port, so a local server the signed-in browser
// visits sees it.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

const (
	// SessionMaxAge is how long a browser stays signed in.
	SessionMaxAge = 30 * 24 * time.Hour
	// SignInCodeTTL is how long a one-time sign-in code can be used.
	SignInCodeTTL = time.Minute
	// maxSignInCodes bounds the codes waiting to be used.
	maxSignInCodes = 64

	signInPath    = "/app/sign-in"
	signInAPIPath = "/api/sign-in"
)

// sessionCookieName is the session cookie of aimd on port.
func sessionCookieName(port int) string { return "aim_session_" + strconv.Itoa(port) }

// signIn holds the session key and the one-time codes not used yet.
type signIn struct {
	key    []byte
	cookie string

	mu    sync.Mutex
	codes map[string]time.Time // code → when it stops working
}

func newSignIn(secret string, port int) *signIn {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("aim session v1"))
	return &signIn{key: m.Sum(nil), cookie: sessionCookieName(port), codes: map[string]time.Time{}}
}

// newCode returns a code that signs one browser in, once, until
// SignInCodeTTL passes.
func (s *signIn) newCode(now time.Time) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("web: crypto/rand failed: " + err.Error())
	}
	code := base64.RawURLEncoding.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	for c, until := range s.codes {
		if !now.Before(until) {
			delete(s.codes, c)
		}
	}
	if len(s.codes) >= maxSignInCodes {
		// Someone with the token is minting codes in a loop: the unused
		// ones go, so the map stays small.
		clear(s.codes)
	}
	s.codes[code] = now.Add(SignInCodeTTL)
	return code
}

// useCode reports whether code is one newCode gave and nobody used, and
// uses it up.
func (s *signIn) useCode(code string, now time.Time) bool {
	if code == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.codes[code]
	delete(s.codes, code)
	return ok && now.Before(until)
}

// hasToken reports whether a request carries aimd's token.
func (s *Server) hasToken(r *http.Request) bool {
	tok := r.Header.Get(TokenHeader)
	return tok != "" && s.opts.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.Token)) == 1
}

// hasSession reports whether a request carries a session cookie this aimd
// set less than SessionMaxAge ago.
func (s *Server) hasSession(r *http.Request) bool {
	c, err := r.Cookie(s.signIn.cookie)
	return err == nil && validSigned(s.signIn.key, c.Value, s.now(), SessionMaxAge)
}

// openPath reports whether a request needs no credential: a static file,
// or the sign-in link (which carries its own, the code).
func openPath(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == signInPath
}

// requireSignIn refuses (401) any request without aimd's token or a
// session cookie, but those openPath lets through. A program gets
// {"error": …}, a browser a page saying how to sign in.
func (s *Server) requireSignIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if openPath(r) || s.hasToken(r) || s.hasSession(r) {
			next.ServeHTTP(w, r)
			return
		}
		if isAPIPath(r.URL.Path) || !safeMethod(r.Method) {
			writeError(w, http.StatusUnauthorized, "not signed in: send aimd's token in "+TokenHeader+", or run `aim open` to sign this browser in")
			return
		}
		signedOut(w, "This browser isn't signed in to ai-manager.")
	})
}

// signedOut answers 401 with the page a browser without a session gets. It
// holds nothing of aimd's: no data and no CSRF token.
func signedOut(w http.ResponseWriter, why string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ai-manager: sign in</title>
<link rel="icon" href="/static/icon.svg">
<h1>ai-manager</h1>
<p>` + why + `</p>
<p>Run <code>aim open</code> in a terminal on this Mac. It opens the dashboard and signs this browser in for 30 days.</p>
</html>
`))
}

// postSignIn is POST /api/sign-in: a one-time link that signs a browser
// in. Like every write under /api/ it needs aimd's token (apiAuth).
func (s *Server) postSignIn(w http.ResponseWriter, r *http.Request) {
	var req api.SignInRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	q := url.Values{"code": {s.signIn.newCode(s.now())}}
	if to := appLocation(req.Return); to != "/app/" {
		q.Set("return", to)
	}
	writeJSON(w, http.StatusOK, api.SignInLink{Path: signInPath + "?" + q.Encode(), ExpiresIn: int(SignInCodeTTL / time.Second)})
}

// getSignIn is GET /app/sign-in?code=…&return=…: it trades a code from
// POST /api/sign-in for the session cookie and sends the browser on to
// return (a path in the app). A used or expired code signs nobody in; a
// browser that is signed in already is sent on all the same, so an old
// link still opens its page.
func (s *Server) getSignIn(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	to := appLocation(q.Get("return"))
	w.Header().Set("Cache-Control", "no-store")
	if !s.signIn.useCode(q.Get("code"), s.now()) {
		if s.hasSession(r) {
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
		signedOut(w, "This sign-in link was used already or is over a minute old.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.signIn.cookie,
		Value:    newSigned(s.signIn.key, s.now()),
		Path:     "/",
		MaxAge:   int(SessionMaxAge / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, to, http.StatusSeeOther)
}
