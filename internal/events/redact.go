package events

import (
	"regexp"
	"strings"
)

// Redacted replaces each secret Redact finds.
const Redacted = "[redacted]"

// redactions are the secrets Redact replaces, in order. plugin/mod/wait.ts
// has the same patterns in the same order (its REDACTIONS); change both.
var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	// An env-style assignment whose name says it holds a secret:
	// GITHUB_TOKEN=…, export FOO_KEY=…, ?api_key=…. The name starts a
	// word (so --author=x is a flag, not an assignment).
	{regexp.MustCompile(`(?i)(^|[\s;&|(?,{"'])([A-Za-z_][A-Za-z0-9_.-]*(?:key|token|secret|passw(?:or)?d|pwd|auth|credential|cookie)[A-Za-z0-9_.-]*)(\s*=\s*)("[^"]*"|'[^']*'|[^\s"',;&|)]+)`), "${1}${2}${3}" + Redacted},
	// Authorization and Cookie header values, to the end of the quoted
	// argument or the line.
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization|(?:set-)?cookie)(\s*:\s*)([^'"\r\n]+)`), "${1}${2}" + Redacted},
	// Any other -H / --header whose name says it holds a secret.
	{regexp.MustCompile(`(?i)((?:^|\s)(?:-H|--header)(?:\s+|=)['"]?[A-Za-z0-9-]*(?:key|token|secret|auth|cookie|credential|passw(?:or)?d)[A-Za-z0-9-]*\s*:\s*)([^'"\r\n]+)`), "${1}" + Redacted},
	// A --password / --token style flag's value.
	{regexp.MustCompile(`(?i)((?:^|\s)--?(?:password|passwd|token|secret|api-key|apikey|access-key|secret-key|private-key|auth-token|client-secret)(?:\s+|=))("[^"]*"|'[^']*'|[^\s"']+)`), "${1}" + Redacted},
	// A Bearer or Basic credential on its own.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{12,}`), "${1} " + Redacted},
	// A user:password@ in a URL.
	{regexp.MustCompile(`(://)[^/\s:@]+:[^/\s@]+@`), "${1}" + Redacted + "@"},
	// Well-known token shapes.
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|xox[abposr]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16})\b`), Redacted},
}

// longRun is a run of 32 or more base64 or hex characters after = or :.
var longRun = regexp.MustCompile(`([=:]\s*)([A-Za-z0-9+_-]{32,}={0,2})`)

// Redact replaces the secrets it can spot in s with "[redacted]": a net
// for the obvious cases, not a guarantee. The mod runs the same rules
// before a wait leaves the session (plugin/mod/wait.ts), and aimd runs
// them again on every wait it receives (Normalize), for older mods.
//
//   - an env-style assignment whose name contains KEY, TOKEN, SECRET,
//     PASSWORD, PASSWD, PWD, AUTH, CREDENTIAL or COOKIE (any case);
//   - Authorization and Cookie header values, and -H / --header values
//     whose name says it holds a secret;
//   - --password / --token style flag values;
//   - a Bearer or Basic credential;
//   - user:password@ in a URL;
//   - ghp_, gho_, ghs_, ghu_, ghr_ and github_pat_ tokens, sk-…, xox[abposr]-…,
//     AWS AKIA… keys;
//   - a run of 32 or more base64 or hex characters, with a letter and a
//     digit in it, after = or :.
//
// It is idempotent: what it leaves, it leaves again.
func Redact(s string) string {
	if s == "" {
		return s
	}
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return longRun.ReplaceAllStringFunc(s, func(m string) string {
		sub := longRun.FindStringSubmatch(m)
		if !strings.ContainsAny(sub[2], "0123456789") || !strings.ContainsAny(strings.ToLower(sub[2]), "abcdefghijklmnopqrstuvwxyz") {
			return m // a long name or a number, not a secret
		}
		return sub[1] + Redacted
	})
}
