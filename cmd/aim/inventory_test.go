package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/prune/prunetest"
)

func TestMain(m *testing.M) { os.Exit(prunetest.Main(m)) }

// world builds a prune test world, points AIM_HOME at a config whose only
// root is it, and makes aim use the world's fake GitHub, sessions and
// processes. Tests using it can't run in parallel.
func world(t *testing.T) *prunetest.World {
	t.Helper()
	w := prunetest.BuildWorld(t)
	home := filepath.Join(prunetest.TempDir(t), "aim")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ConfigPath(home), []byte(w.ConfigTOML()), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.HomeEnv, home)
	old := newChecker
	t.Cleanup(func() { newChecker = old })
	newChecker = func(cfg config.Config) (*prune.Checker, error) {
		c, err := defaultChecker(cfg)
		if err != nil {
			return nil, err
		}
		c.GitHub, c.Sessions, c.Procs = w.GitHub, w.Sessions, w.Procs
		return c, nil
	}
	return w
}

// aim runs the CLI with input on stdin.
func aim(t *testing.T, input string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	old := stdin
	stdin = strings.NewReader(input)
	defer func() { stdin = old }()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// lineFor returns the indented table row for path.
func lineFor(t *testing.T, out, path string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == path && strings.HasPrefix(l, "  ") {
			return l
		}
	}
	t.Errorf("no line for %s in:\n%s", path, out)
	return ""
}

// actionLine returns the --apply report line "<verb> <path> ...".
func actionLine(out, verb, path string) string {
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[0] == verb && f[1] == path {
			return l
		}
	}
	return ""
}

func TestLs(t *testing.T) {
	w := world(t)
	code, out, errOut := aim(t, "", "ls")
	if code != 0 || errOut != "" {
		t.Fatalf("aim ls = %d, stderr %q", code, errOut)
	}
	for name, want := range map[string][]string{
		"merged":         {" merged ", " 0 ", " none ", "#1 merged", "safe: PR #1 merged into main"},
		"atrisk":         {"#6 merged", "not safe: 1 ignored path at risk: scripts/.spike-state/"},
		"dirty":          {"not safe: 1 uncommitted file; 1 untracked file"},
		"open":           {"#71 open", "not safe: PR #71 is open"},
		"closed":         {"#8 closed", "safe with --include-closed: PR #8 closed without merging"},
		"stalegh":        {" ? ", "PR unknown"},
		"lfs":            {" ? ", "not safe: status unknown"},
		"locked-live":    {"locked by agent a1111111111111111 of lead pid 111"},
		"locked-stale":   {"stale lock"},
		"detached-main":  {"(detached " + w.C0[:7] + ")", "scratch: detached at " + w.C0[:7] + ", on origin/main"},
		"gone":           {"prunable: gitdir file points to non-existent location"},
		"main":           {"main checkout"},
		"plain-feature":  {"not safe: no GitHub remote"},
		"scratch-live":   {"in the scratchpad of live"},
		"detached-local": {"isn't on origin/main or origin/surge"},
	} {
		line := lineFor(t, out, w.WT[name])
		for _, s := range want {
			if !strings.Contains(line, s) {
				t.Errorf("%s: line %q lacks %q", name, line, s)
			}
		}
	}
	for _, h := range []string{w.Repo + "  (acme/widgets)", w.Plain + "  (no GitHub remote)"} {
		if !strings.Contains(out, h+"\n") {
			t.Errorf("no header %q", h)
		}
	}
	if !strings.Contains(out, "PATH") || !strings.Contains(out, "IGNORED AT RISK") {
		t.Error("no column headers")
	}
}

func TestLsJSON(t *testing.T) {
	w := world(t)
	code, out, errOut := aim(t, "", "ls", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("aim ls --json = %d, stderr %q", code, errOut)
	}
	var got lsJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(got.Repos) != 2 || got.LiveSessions != 3 {
		t.Fatalf("%d repos, %d live sessions", len(got.Repos), got.LiveSessions)
	}
	seen := 0
	for _, r := range got.Repos {
		for _, wt := range r.Worktrees {
			name := w.Name(wt.Path)
			seen++
			if wt.Verdict.Category != w.Want[name] {
				t.Errorf("%s: category %s, want %s", name, wt.Verdict.Category, w.Want[name])
			}
			if wt.Verdict.Safe != (w.Want[name] == "safe" || w.Want[name] == "scratch") {
				t.Errorf("%s: safe = %v", name, wt.Verdict.Safe)
			}
			switch name {
			case "atrisk":
				if wt.Ignored == nil || len(wt.Ignored.AtRisk) != 1 || wt.Ignored.AtRisk[0] != "scripts/.spike-state/" || wt.Ignored.Rebuildable != 1 {
					t.Errorf("atrisk ignored = %+v", wt.Ignored)
				}
			case "merged":
				if wt.PR == nil || wt.PR.Number != 1 || wt.PR.State != "merged" || wt.PR.HeadOID != w.Head["merged"] || wt.Head != w.Head["merged"] {
					t.Errorf("merged PR = %+v, head %s", wt.PR, wt.Head)
				}
				if wt.Ignored == nil || wt.Ignored.AtRisk == nil || len(wt.Ignored.AtRisk) != 0 {
					t.Errorf("merged ignored = %+v (at_risk must be [] not null)", wt.Ignored)
				}
			case "locked-stale":
				if wt.Lock == nil || wt.Lock.State != "stale" || wt.Lock.PID != prunetest.StalePID {
					t.Errorf("locked-stale lock = %+v", wt.Lock)
				}
			case "lfs":
				if wt.StatusKnown {
					t.Error("lfs: status_known true")
				}
			case "detached-surge":
				if wt.ReachableFrom != "origin/surge" {
					t.Errorf("detached-surge reachable_from = %q", wt.ReachableFrom)
				}
			}
		}
	}
	if seen != len(w.Want) {
		t.Errorf("%d worktrees in JSON, want %d", seen, len(w.Want))
	}
}

func TestLsRepoAndFlags(t *testing.T) {
	w := world(t)
	code, out, _ := aim(t, "", "ls", "--repo", "plain")
	if code != 0 || strings.Contains(out, w.Repo) || !strings.Contains(out, w.WT["plain-feature"]) {
		t.Errorf("aim ls --repo plain = %d:\n%s", code, out)
	}
	if code, _, errOut := aim(t, "", "ls", "--repo", "nope"); code != 1 || !strings.Contains(errOut, "no repo matches") {
		t.Errorf("aim ls --repo nope = %d, %q", code, errOut)
	}
	if code, _, _ := aim(t, "", "ls", "--bogus"); code != 2 {
		t.Errorf("aim ls --bogus = %d", code)
	}
	if code, _, _ := aim(t, "", "ls", "extra"); code != 2 {
		t.Errorf("aim ls extra = %d", code)
	}
	if code, _, errOut := aim(t, "", "ls", "-h"); code != 0 || !strings.Contains(errOut, "usage: aim ls") {
		t.Errorf("aim ls -h = %d, %q", code, errOut)
	}
}

func TestLsWarnings(t *testing.T) {
	w := world(t)
	w.Sessions.Err = os.ErrNotExist
	code, _, errOut := aim(t, "", "ls")
	if code != 0 || !strings.Contains(errOut, "live sessions unknown") {
		t.Errorf("aim ls without sessions = %d, stderr %q", code, errOut)
	}
}

func TestConfigError(t *testing.T) {
	home := prunetest.TempDir(t)
	t.Setenv(config.HomeEnv, home)
	if err := os.WriteFile(config.ConfigPath(home), []byte("[prune]\nrebuildable = [\"../x\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"ls", "prune"} {
		if code, _, errOut := aim(t, "", cmd); code != 1 || !strings.Contains(errOut, "prune.rebuildable") {
			t.Errorf("aim %s with a bad config = %d, %q", cmd, code, errOut)
		}
	}
}
