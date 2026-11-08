package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/focus"
	"github.com/gravexa-llc/ai-manager/internal/osascript"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

const goUsage = `usage: aim go <session>

Brings a Claude Code session to the front in iTerm2 or Terminal:
  - an interactive session: selects its tab (found by its terminal) in
    iTerm2, else in Terminal, and activates that app;
  - a background session: opens it with ` + "`claude attach <id>`" + ` in a new
    iTerm2 tab, or a new Terminal window when iTerm2 isn't running;
  - a subagent: aim can't select it inside its lead, so it goes to the
    lead session and says so.

<session> is a session key (pid:123, job:ab12cd34), a pid, a background
job id, a session id, an agent id (only while aimd runs), or a unique
prefix of a session's name, its cwd or its cwd's last folder.

It asks aimd (POST /api/go) when aimd is running; otherwise it lists the
sessions with ` + "`claude agents --json`" + ` and does it itself.
`

// goWait bounds a Go: long enough for macOS's first "wants to control
// iTerm2" (or Terminal) prompt to be answered.
const goWait = 30 * time.Second

// goSys is what `aim go` touches outside the process, so tests run no
// osascript and need no aimd.
type goSys struct {
	// post sends body to url with the token, and returns the status and
	// the answer.
	post func(ctx context.Context, url, token string, body []byte) (int, []byte, error)
	// direct is Go without aimd.
	direct func() *focus.Service
}

func realGoSys() goSys {
	return goSys{post: postJSON, direct: directGo}
}

func cmdGo(args []string, stdout, stderr io.Writer) int {
	return goCmd(realGoSys(), args, stdout, stderr)
}

func goCmd(s goSys, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		fmt.Fprint(stdout, goUsage)
		return 0
	}
	if len(args) != 1 || args[0] == "" {
		fmt.Fprint(stderr, goUsage)
		return 2
	}
	key := args[0]
	home, err := config.Home()
	if err != nil {
		fmt.Fprintf(stderr, "aim go: %v\n", err)
		return 1
	}
	cfg, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "aim go: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), goWait+5*time.Second)
	defer cancel()

	// aimd creates its token when it starts, so no token means no aimd.
	token, err := config.ReadSecretFile(config.TokenPath(home))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		fmt.Fprintf(stderr, "aim go: %v\n", err)
		return 1
	default:
		body, _ := json.Marshal(api.GoRequest{Key: key})
		status, resp, err := s.post(ctx, goURL(cfg.Port), token, body)
		if err == nil {
			return printGoAnswer(status, resp, stdout, stderr)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			fmt.Fprintf(stderr, "aim go: aimd on 127.0.0.1:%d: %v\n", cfg.Port, err)
			return 1
		}
	}

	fmt.Fprintln(stderr, "aimd isn't running; going directly (sessions from `claude agents --json`)")
	svc := s.direct()
	res, err := svc.Go(ctx, key)
	if err != nil {
		var fe *focus.Error
		if errors.As(err, &fe) {
			printRefusal(api.GoError{Error: fe.Msg, Candidates: fe.Candidates}, stderr)
		} else {
			fmt.Fprintf(stderr, "aim go: %v\n", err)
		}
		return 1
	}
	fmt.Fprintln(stdout, res.Message)
	return 0
}

// goURL is aimd's Go endpoint on port.
func goURL(port int) string { return fmt.Sprintf("http://127.0.0.1:%d/api/go", port) }

// printGoAnswer prints what aimd answered and returns the exit code.
func printGoAnswer(status int, body []byte, stdout, stderr io.Writer) int {
	if status == http.StatusOK {
		var res api.GoResult
		if err := json.Unmarshal(body, &res); err != nil {
			fmt.Fprintf(stderr, "aim go: unreadable answer from aimd: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, res.Message)
		return 0
	}
	var refusal api.GoError
	if err := json.Unmarshal(body, &refusal); err != nil || refusal.Error == "" {
		if status == http.StatusNotFound {
			fmt.Fprintln(stderr, "aim go: this aimd has no /api/go; rebuild and restart it (make build && aim up)")
			return 1
		}
		fmt.Fprintf(stderr, "aim go: aimd answered HTTP %d\n", status)
		return 1
	}
	printRefusal(refusal, stderr)
	return 1
}

// printRefusal prints a refusal, and the candidates of an ambiguous key.
func printRefusal(e api.GoError, w io.Writer) {
	fmt.Fprintf(w, "aim go: %s\n", e.Error)
	for _, c := range e.Candidates {
		line := "  " + c.Key + "  " + c.Title
		if c.Cwd != "" {
			line += "  " + c.Cwd
		}
		fmt.Fprintln(w, line)
	}
}

// postJSON posts body to url with aimd's token.
func postJSON(ctx context.Context, url, token string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-AIM-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, err
}

// directGo is Go without aimd: sessions from `claude agents --json`,
// claude's path from PATH.
func directGo() *focus.Service {
	claude, err := exec.LookPath("claude")
	if err == nil {
		claude, err = filepath.Abs(claude)
	}
	if err != nil {
		claude = ""
	}
	userHome, _ := os.UserHomeDir()
	return &focus.Service{
		Source:   focus.ListerSource{Lister: sessions.Lister{ActiveOnly: true}},
		Terminal: term.Terminal{Runner: osascript.Exec{Timeout: goWait}, TTYs: term.PS{}, Claude: claude},
		HomeDir:  userHome,
	}
}
