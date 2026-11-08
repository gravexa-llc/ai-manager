package plans

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The plugin's skills teach this package's format (plugin/skills, C5).
// These tests keep the two in sync: no skill file may itself read as a plan,
// and plan-write's example plan must parse exactly as the skill says.

var skillsDir = filepath.Join("..", "..", "plugin", "skills")

// skillFiles returns every plugin/skills/*/SKILL.md, keyed by its folder.
func skillFiles(t *testing.T) map[string][]byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(skillsDir, "*", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(filepath.Dir(p))] = b
	}
	for _, want := range []string{"plan-write", "plan-status", "plan-join"} {
		if files[want] == nil {
			t.Errorf("missing skill %s/SKILL.md", want)
		}
	}
	return files
}

func TestSkillsAreNotPlans(t *testing.T) {
	for name, src := range skillFiles(t) {
		if d := Detect(src); d.IsPlan {
			t.Errorf("%s: Detect says it's a plan (%d packages); keep example tables inside a fenced code block", name, d.Packages)
		}
		if p := Parse(src); len(p.Packages) > 0 {
			t.Errorf("%s: has package rows outside a code block: %v", name, ids(p))
		}
	}
}

func TestSkillFrontmatter(t *testing.T) {
	for name, src := range skillFiles(t) {
		front, ok := frontmatter(string(src))
		if !ok {
			t.Errorf("%s: no YAML frontmatter", name)
			continue
		}
		if front["name"] != name {
			t.Errorf("%s: frontmatter name is %q, want the folder's name", name, front["name"])
		}
		if front["description"] == "" {
			t.Errorf("%s: frontmatter has no description", name)
		}
	}
}

// frontmatter reads the flat key: value lines between the leading --- lines.
func frontmatter(src string) (map[string]string, bool) {
	rest, ok := strings.CutPrefix(src, "---\n")
	if !ok {
		return nil, false
	}
	block, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return nil, false
	}
	out := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out, true
}

// fencedBlocks returns the contents of every ``` fenced block whose info
// string is info.
func fencedBlocks(src, info string) []string {
	var blocks []string
	var cur []string
	in := false
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case !in && trimmed == "```"+info:
			in, cur = true, nil
		case in && trimmed == "```":
			in = false
			blocks = append(blocks, strings.Join(cur, "\n")+"\n")
		case in:
			cur = append(cur, line)
		}
	}
	return blocks
}

func TestPlanWriteExample(t *testing.T) {
	src := skillFiles(t)["plan-write"]
	blocks := fencedBlocks(string(src), "markdown")
	if len(blocks) != 1 {
		t.Fatalf("plan-write has %d ```markdown blocks, want exactly 1 (the example plan)", len(blocks))
	}
	example := []byte(blocks[0])

	if d := Detect(example); !d.IsPlan {
		t.Fatalf("the example isn't detected as a plan: %+v", d)
	}
	p := Parse(example)
	for _, w := range p.Warnings {
		t.Errorf("warning: %s", w)
	}
	if p.Title != "Dark mode: dispatch plan" {
		t.Errorf("title = %q", p.Title)
	}
	// The file name would give "dark-mode"; the Plan slug: line wins.
	if got := Slug("docs/dark-mode-plan.md", example); got != "dark" {
		t.Errorf("slug = %q, want dark", got)
	}
	if p.Merges != MergesYou || !strings.Contains(string(example), "\nMerges: you\n") {
		t.Errorf("merges = %q, want the example's Merges: you line", p.Merges)
	}
	// The skill teaches both values, and the Merges line itself.
	for _, part := range []string{"`Merges: you`", "`Merges: lead`"} {
		if !strings.Contains(string(src), part) {
			t.Errorf("plan-write doesn't mention %s", part)
		}
	}
	if m, _ := Merges(src); m != MergesYou {
		t.Errorf("the skill file itself reads as merges %q", m)
	}

	checkPackages(t, p, []pkgWant{
		{ID: "T1", Title: "Color tokens", Repo: "app", Size: "M", Done: true, Lane: "Theme"},
		{ID: "T2", Title: "Dark palette", Repo: "app", Size: "S", Needs: "T1", Lane: "Theme"},
		{ID: "W1", Title: "Site theme toggle", Repo: "web", Size: "S", Lane: "Site"},
		{ID: "D1", Title: "Pick the accent", Size: "S"},
		{ID: "Q1", Title: "Screenshot pass", Needs: "T2,W1,D1",
			Note: "you: check on a real device", NeedsYou: "check on a real device"},
		{ID: "R1", Title: "Release", Needs: "Q1", Note: "once the beta has run a week"},
	})
	for _, pkg := range p.Packages {
		if want := pkg.ID == "D1"; pkg.Yours != want {
			t.Errorf("%s: Yours = %v, want %v", pkg.ID, pkg.Yours, want)
		}
	}

	type laneWant struct {
		Name         string
		Packages     []string
		SingleWriter bool
	}
	var lanes []laneWant
	for _, l := range p.Lanes {
		lanes = append(lanes, laneWant{l.Name, l.Packages, l.SingleWriter})
	}
	wantLanes := []laneWant{
		{"Theme", []string{"T1", "T2"}, true},
		{"Site", []string{"W1"}, false},
	}
	if !reflect.DeepEqual(lanes, wantLanes) {
		t.Errorf("lanes:\n got %+v\nwant %+v", lanes, wantLanes)
	}
}
