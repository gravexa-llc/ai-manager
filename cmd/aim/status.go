package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

const statusUsage = `usage: aim status

Prints one line from aimd's /api/status: sessions and PRs that need you,
stale sources, GitHub repos aimd can't read, the GitHub budget, and the
version of the aimd that is running, with a note when it isn't this aim's
(aim up restarts it on the aimd beside aim). Exits 1 when aimd isn't
running.
`

// statusTimeout bounds one status request.
const statusTimeout = 2 * time.Second

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	return status(realSys(), args, stdout, stderr)
}

func status(s sys, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
			fmt.Fprint(stdout, statusUsage)
			return 0
		}
		fmt.Fprint(stderr, statusUsage)
		return 2
	}
	home, err := config.Home()
	if err != nil {
		fmt.Fprintf(stderr, "aim status: %v\n", err)
		return 1
	}
	cfg, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "aim status: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
	defer cancel()
	body, err := s.httpGet(ctx, statusURL(cfg.Port))
	if err != nil {
		fmt.Fprintf(stdout, "aimd not running (127.0.0.1:%d)\n", cfg.Port)
		return 1
	}
	var st api.Status
	if err := json.Unmarshal(body, &st); err != nil {
		fmt.Fprintf(stderr, "aim status: unreadable answer from aimd: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, statusLine(st, version))
	return 0
}

// statusURL is aimd's status endpoint on port.
func statusURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/api/status", port)
}

// statusLine summarizes a status in one line, short enough for an iTerm2
// status bar, ending with the running aimd's version (versionPart; aim is
// this aim's own):
//
//	2 sessions need you (oldest 12m) · 1 PR needs you · stale: none · GitHub 4890/5000 · aimd v1.0.0-14-gabc1234
func statusLine(st api.Status, aim string) string {
	c := st.Counts
	var parts []string

	sess := plural(c.SessionsNeedingYou, "session needs you", "sessions need you")
	if c.SessionsNeedingYou > 0 && !c.OldestNeedsYouSince.IsZero() {
		sess += " (oldest " + shortAge(st.Now.Sub(c.OldestNeedsYouSince)) + ")"
	}
	parts = append(parts, sess)
	parts = append(parts, plural(c.PRsNeedingYou, "PR needs you", "PRs need you"))

	if len(c.StaleSources) == 0 {
		parts = append(parts, "stale: none")
	} else {
		parts = append(parts, "stale: "+strings.Join(c.StaleSources, ", "))
	}
	// Repos GitHub didn't answer for, while the source as a whole works:
	// their PRs stay as aimd last read them.
	switch un := st.GitHub.Unreadable; len(un) {
	case 0:
	case 1:
		parts = append(parts, "GitHub can't read "+un[0].Repo)
	default:
		parts = append(parts, fmt.Sprintf("GitHub can't read %d repos", len(un)))
	}

	b := st.GitHub.Budget
	switch {
	case !b.Known:
		parts = append(parts, "GitHub budget unknown")
	case b.Verdict == "stop":
		g := fmt.Sprintf("GitHub %d/%d stopped", b.Remaining, b.Limit)
		if !b.ResumeAt.IsZero() {
			g += " until " + b.ResumeAt.Local().Format("15:04")
		}
		parts = append(parts, g)
	case b.Verdict == "idle-only":
		parts = append(parts, fmt.Sprintf("GitHub %d/%d idle-only", b.Remaining, b.Limit))
	default:
		parts = append(parts, fmt.Sprintf("GitHub %d/%d", b.Remaining, b.Limit))
	}
	parts = append(parts, versionPart(st.Version, aim))
	return strings.Join(parts, " · ")
}

// versionPart names the aimd that answered: "aimd v1.0.0-14-gabc1234".
// `aim --version` is the aim binary on disk, which an update replaces
// while the old aimd keeps running, so when the two differ it says how to
// switch: "aimd v1.0.0-12-g… is running; aim is v1.0.0-14-g…; run aim up
// to restart it". A dev build (go run, go test) is never compared.
func versionPart(running, aim string) string {
	switch {
	case running == "":
		return "aimd version unknown"
	case knownVersion(running) && knownVersion(aim) && running != aim:
		return "aimd " + running + " is running; aim is " + aim + "; run aim up to restart it"
	}
	return "aimd " + running
}

// knownVersion reports whether v names a build: not empty, not "dev".
func knownVersion(v string) bool { return v != "" && v != "dev" }

// plural writes "1 PR needs you" or "3 PRs need you".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// shortAge writes a duration as 45s, 12m, 3h or 4d.
func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d/time.Second)))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}

// httpGet fetches url and returns the body of a 200 answer.
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}
