package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"strconv"
	"time"
)

// CSRF form field and header names, and how long a token stays valid.
const (
	CSRFField  = "csrf_token"
	CSRFHeader = "X-CSRF-Token"
	CSRFMaxAge = 12 * time.Hour
)

// CSRF protects the dashboard's own forms. Each page gets a fresh
// token from Token and puts it in a hidden CSRFField input (or, from
// script, in the CSRFHeader header). Protect refuses any write that doesn't
// carry a valid one.
//
// A token is a random nonce and its issue time, signed with HMAC-SHA256
// under a key derived from aimd's token, so tokens survive a restart of
// aimd and can't be forged by another page on localhost (cookies don't
// separate ports, so a cookie-based double submit could be). A cross-site
// page can't read a token out of a dashboard page: aimd sends no CORS
// headers and checks the Host header. Nor can a local process without a
// credential: a page is served only to a signed-in browser (signin.go).
type CSRF struct {
	key     []byte
	origins map[string]bool
	now     func() time.Time
}

// NewCSRF returns the protection for aimd on port, keyed by secret
// (aimd's token).
func NewCSRF(secret string, port int) *CSRF {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("aim csrf v1"))
	p := strconv.Itoa(port)
	return &CSRF{
		key:     m.Sum(nil),
		origins: map[string]bool{"http://127.0.0.1:" + p: true, "http://localhost:" + p: true},
		now:     time.Now,
	}
}

const (
	csrfNonceLen = 16
	csrfTimeLen  = 8
	csrfMACLen   = sha256.Size
)

// Token returns a new token for one page.
func (c *CSRF) Token() string { return newSigned(c.key, c.now()) }

// Valid reports whether tok is a token this aimd issued less than
// CSRFMaxAge ago.
func (c *CSRF) Valid(tok string) bool { return validSigned(c.key, tok, c.now(), CSRFMaxAge) }

// newSigned returns a random nonce and the time now, signed with
// HMAC-SHA256 under key: a CSRF token, or a session cookie's value
// (signin.go), each under its own key.
func newSigned(key []byte, now time.Time) string {
	buf := make([]byte, csrfNonceLen+csrfTimeLen, csrfNonceLen+csrfTimeLen+csrfMACLen)
	if _, err := rand.Read(buf[:csrfNonceLen]); err != nil {
		panic("web: crypto/rand failed: " + err.Error())
	}
	binary.BigEndian.PutUint64(buf[csrfNonceLen:], uint64(now.Unix()))
	buf = append(buf, signedMAC(key, buf)...)
	return base64.RawURLEncoding.EncodeToString(buf)
}

func (c *CSRF) mac(b []byte) []byte { return signedMAC(c.key, b) }

func signedMAC(key, b []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(b)
	return m.Sum(nil)
}

// validSigned reports whether tok is a newSigned value under key, issued
// less than maxAge before now.
func validSigned(key []byte, tok string, now time.Time, maxAge time.Duration) bool {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(b) != csrfNonceLen+csrfTimeLen+csrfMACLen {
		return false
	}
	body, sig := b[:csrfNonceLen+csrfTimeLen], b[csrfNonceLen+csrfTimeLen:]
	if !hmac.Equal(sig, signedMAC(key, body)) {
		return false
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(body[csrfNonceLen:])), 0)
	age := now.Sub(issued)
	return age >= -time.Minute && age < maxAge
}

// maxFormBytes bounds a form body read for its token.
const maxFormBytes = 1 << 20

// nullSameOrigin reports whether a request carries "Origin: null" but the
// browser says it is same-origin. That is what a dashboard form post looks
// like: the pages are served with Referrer-Policy: no-referrer, and the
// Fetch standard then serializes the origin of a form submission as null.
// Other sources of a null origin (sandboxed frames, data: URLs, redirects
// from elsewhere) are cross-site by Sec-Fetch-Site, and still need a token.
func nullSameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == "null" && r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// Protect refuses (403) any write that comes from another origin, by its
// Origin or Sec-Fetch-Site header, or that lacks a valid token in the
// CSRFHeader header or the CSRFField form field. Reads pass. "Origin:
// null" passes the origin check only with "Sec-Fetch-Site: same-origin"
// (see nullSameOrigin).
func (c *CSRF) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !c.origins[o] && !nullSameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
			writeError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		tok := r.Header.Get(CSRFHeader)
		if tok == "" {
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
			tok = r.PostFormValue(CSRFField)
		}
		if !c.Valid(tok) {
			writeError(w, http.StatusForbidden, "missing or expired form token; reload the page")
			return
		}
		next.ServeHTTP(w, r)
	})
}
