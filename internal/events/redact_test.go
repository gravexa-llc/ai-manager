package events

import "testing"

// redactCases are the examples plugin/mod/wait.test.ts checks too
// (REDACT_CASES there): change both together.
var redactCases = []struct{ in, want string }{
	// Env-style assignments whose name says it holds a secret.
	{"GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123 gh pr list", "GITHUB_TOKEN=[redacted] gh pr list"},
	{"export FOO_KEY=abc123; npm test", "export FOO_KEY=[redacted]; npm test"},
	{"DB_PASSWORD='hunter 2' ./run", "DB_PASSWORD=[redacted] ./run"},
	{`aws_secret_access_key="xyz" make`, "aws_secret_access_key=[redacted] make"},
	{"PGPASSWD=x OLDPWD=/tmp env", "PGPASSWD=[redacted] OLDPWD=[redacted] env"},
	{"MY_AUTH=a SESSION_COOKIE=b AZURE_CREDENTIAL=c", "MY_AUTH=[redacted] SESSION_COOKIE=[redacted] AZURE_CREDENTIAL=[redacted]"},
	{"curl https://x.test/api?api_key=s3cr3t&page=2", "curl https://x.test/api?api_key=[redacted]&page=2"},
	// Header values, and -H arguments carrying a secret.
	{"curl -H 'Authorization: Bearer abc.def' https://x.test", "curl -H 'Authorization: [redacted]' https://x.test"},
	{`curl -H "Cookie: sid=123; theme=dark" x.test`, `curl -H "Cookie: [redacted]" x.test`},
	{"curl --header 'X-Api-Key: abc' x.test", "curl --header 'X-Api-Key: [redacted]' x.test"},
	{"curl -H 'X-Auth-Token: zzz' x.test", "curl -H 'X-Auth-Token: [redacted]' x.test"},
	// A credential in a URL.
	{"git clone https://user:pa55@github.com/o/r.git", "git clone https://[redacted]@github.com/o/r.git"},
	// Well-known token shapes, wherever they are.
	{"echo ghp_abcdefghijklmnopqrstuvwxyz0123", "echo [redacted]"},
	{"use gho_abcdefghijklmnopqrstuvwx and ghs_abcdefghijklmnopqrstuvwx", "use [redacted] and [redacted]"},
	{"token github_pat_11ABCDEFG0123456789_abcdefghij", "token [redacted]"},
	{"OPENAI sk-abcdefghijklmnopqrstuvwx1234", "OPENAI [redacted]"},
	{"slack xoxb-123456789012-abcdef", "slack [redacted]"},
	{"aws AKIAABCDEFGHIJKLMNOP", "aws [redacted]"},
	// Long base64 or hex runs after = or :.
	{"--sig=0123456789abcdef0123456789abcdef", "--sig=[redacted]"},
	{"value: QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo0NTY3ODk=", "value: [redacted]"},
	// --password / --token style flags.
	{"mysql --password hunter2 db", "mysql --password [redacted] db"},
	{"cli --token=abc --api-key xyz", "cli --token=[redacted] --api-key [redacted]"},

	// Near misses: left alone.
	{"git log --author=sam", "git log --author=sam"},
	{"author: Jane Doe", "author: Jane Doe"},
	{"echo $GITHUB_TOKEN | wc -c", "echo $GITHUB_TOKEN | wc -c"},
	{"gh auth token --user example-user", "gh auth token --user example-user"},
	{"cat --token-file ~/.ai-manager/token", "cat --token-file ~/.ai-manager/token"},
	{"pip install sk-learn ghp_short", "pip install sk-learn ghp_short"},
	{"PATH=/usr/local/bin:/usr/bin make check", "PATH=/usr/local/bin:/usr/bin make check"},
	{"ls /tmp:/Users/me/Projects/ai-manager/internal/live/state", "ls /tmp:/Users/me/Projects/ai-manager/internal/live/state"},
	{"go test -run=TestReplayInteractiveDialogsWaitingOnEverything", "go test -run=TestReplayInteractiveDialogsWaitingOnEverything"},
	{"The Authorization header is missing", "The Authorization header is missing"},
	{"Reset the password for the test user", "Reset the password for the test user"},
	{"mkdir perm-main && echo done", "mkdir perm-main && echo done"},
}

func TestRedact(t *testing.T) {
	for _, c := range redactCases {
		got := Redact(c.in)
		if got != c.want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
		if again := Redact(got); again != got {
			t.Errorf("Redact isn't idempotent on %q: %q", got, again)
		}
	}
}

// aimd redacts a wait again on receipt, whatever the sender did.
func TestNormalizeRedactsTheWait(t *testing.T) {
	e := Event{Kind: NeedsYou, SessionID: "s", At: at, Wait: &Wait{Kind: WaitPermission, Tool: "Bash", Summary: "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123 gh pr list"}}
	if err := e.Normalize(); err != nil {
		t.Fatal(err)
	}
	if e.Wait.Summary != "GITHUB_TOKEN=[redacted] gh pr list" {
		t.Errorf("summary = %q", e.Wait.Summary)
	}
	q := Event{Kind: NeedsYou, SessionID: "s", At: at, Wait: &Wait{Kind: WaitQuestion, Questions: []Question{{
		Header: "Key", Question: "Use API_KEY=abc123 or the vault?", Options: []string{"curl -H 'Authorization: Bearer x'", "Vault"},
	}}}}
	if err := q.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := q.Wait.Questions[0]; got.Question != "Use API_KEY=[redacted] or the vault?" || got.Options[0] != "curl -H 'Authorization: [redacted]'" || got.Header != "Key" {
		t.Errorf("question = %+v", got)
	}
	p := Event{Kind: NeedsYou, SessionID: "s", At: at, Wait: &Wait{Kind: WaitPlan, Plan: "1. Set SECRET_TOKEN=abc\n2. Deploy"}}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if p.Wait.Plan != "1. Set SECRET_TOKEN=[redacted]\n2. Deploy" {
		t.Errorf("plan = %q", p.Wait.Plan)
	}
}
