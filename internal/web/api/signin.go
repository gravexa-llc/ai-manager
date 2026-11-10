package api

// SignInRequest is the optional JSON body of POST /api/sign-in.
type SignInRequest struct {
	// Return is the app page the link opens once the browser is signed in
	// ("/app/plans/search"); empty, or anything outside /app/, means the
	// Overview.
	Return string `json:"return,omitempty"`
}

// SignInLink is the answer of POST /api/sign-in.
type SignInLink struct {
	// Path is the one-time link, without scheme and host: the caller puts
	// its own aimd address in front, so the cookie lands on the host name
	// it uses (127.0.0.1 and localhost are different hosts to a browser).
	Path string `json:"path"`
	// ExpiresIn is how many seconds the link works for. It works once.
	ExpiresIn int `json:"expires_in"`
}
