package web

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
)

// TokenHeader carries aimd's token (config.EnsureToken) on writes from the
// mod and the CLI.
const TokenHeader = "X-AIM-Token"

// allowedHosts returns the Host values aimd answers to on port:
// 127.0.0.1:<port> and localhost:<port>, lower case.
func allowedHosts(port int) map[string]bool {
	p := strconv.Itoa(port)
	return map[string]bool{"127.0.0.1:" + p: true, "localhost:" + p: true}
}

// CheckHost refuses (403) any request whose Host header isn't
// 127.0.0.1:<port> or localhost:<port>. A page on another site that
// rebinds its DNS name to 127.0.0.1 still sends its own name as Host, so
// this blocks DNS rebinding.
func CheckHost(port int, next http.Handler) http.Handler {
	ok := allowedHosts(port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[strings.ToLower(r.Host)] {
			http.Error(w, "aimd: unexpected Host header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ContentSecurityPolicy is sent on every response. default-src 'self'
// means no inline script or style and nothing loaded from another origin:
// the dashboard's script and stylesheet are files aimd serves. The other
// directives don't fall back to default-src, so they are spelled out.
const ContentSecurityPolicy = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders sets headers every response carries. No CORS header is
// ever set, so browsers keep other origins from reading responses.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

// safeMethod reports whether a request with this method only reads.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// RequireToken refuses (401) any request that writes (any method but GET,
// HEAD and OPTIONS) unless its X-AIM-Token header holds token. A browser
// can't add that header to a cross-origin request without a CORS preflight,
// which aimd never answers, so a web page can't write either.
func RequireToken(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !safeMethod(r.Method) {
			got := []byte(r.Header.Get(TokenHeader))
			if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
				writeError(w, http.StatusUnauthorized, "missing or wrong "+TokenHeader)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
