package plans

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSuggestRow(t *testing.T) {
	plan, err := os.ReadFile(filepath.Join("..", "..", "testdata", "plans", "ROADMAP.md"))
	if err != nil {
		t.Fatal(err)
	}
	realSharing, err := os.ReadFile(filepath.Join("..", "..", "testdata", "plans", "sharing-plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	sharing := []byte(`# Sharing

| Package | What | Repo | Size | Needs |
|---|---|---|---|---|
| **A1 Tokens** | x | pivot | S | — |

## Later

| ID | Owner | Depends on |
|---|---|---|
| **Q1 Check** | you | A1 |
`)
	for _, tc := range []struct {
		name, src, id, title string
		done                 bool
		want                 string
	}{
		{"the roadmap", string(plan), "W12", "Unregister from the plan page", false, "| **W12 Unregister from the plan page** |  |  |  |"},
		{"done", string(plan), "W12", "Unregister", true, "| **W12 Unregister** ✅ |  |  |  |"},
		{"no title yet", string(plan), "C9", "", false, "| **C9** |  |  |  |"},
		{"the last package table's columns", string(sharing), "Q2", "Alerts", false, "| **Q2 Alerts** |  |  |"},
		{"the sharing plan's last table", string(realSharing), "Q9", "More QA", false, "| **Q9 More QA** |  |  |"},
		{"a pipe and bold in the title", string(plan), "W13", "Pipes | and **bold**  text", false, `| **W13 Pipes \| and bold text** |  |  |  |`},
		{"no package table", "# Notes\n\nNothing here.\n", "X1", "Thing", false, "| **X1 Thing** |  |  |  |"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SuggestRow([]byte(tc.src), tc.id, tc.title, tc.done)
			if got != tc.want {
				t.Errorf("SuggestRow = %q\n          want %q", got, tc.want)
			}
		})
	}
	// The row reads back as the package it suggests, in the file.
	row := SuggestRow(plan, "W99", "Read back", false)
	src := append(append([]byte(nil), plan...), []byte("\n\n| Package | What | Size | Needs |\n|---|---|---|---|\n"+row+"\n")...)
	p, ok := Parse(src).Package("W99")
	if !ok || p.Title != "Read back" || len(p.Needs) != 0 {
		t.Errorf("the suggested row parses as %+v (found %v)", p, ok)
	}
}
