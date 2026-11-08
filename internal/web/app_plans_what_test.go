package web

import (
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// searchPlanMD is a made-up plan with a What column: markup, raw HTML and
// a link, a long cell, and an empty one.
const searchPlanMD = `# Search v2

Plan slug: search-v2

| Package | What | Needs |
|---|---|---|
| **A1 Index builder** | Builds the ` + "`search`" + ` index **nightly** from the _catalog_. | — |
| **A2 Query API** | Answers queries; see [the spec](https://example.com/spec). <script>alert(1)</script> <b>bold?</b> | A1 |
| **A3 Ranking** | ` + longWhat + ` | A1 |
| **A4 Rollout** | | A2, A3 |
`

const longWhat = "Ranks results by a blend of text match, freshness and popularity, with a per-tenant boost list. " +
	"The weights live in `ranking.toml` and reload without a restart. It keeps the old ranker behind a flag " +
	"for a week so the two can be compared on live traffic. Results that tie keep their index order, so pages " +
	"don't reshuffle between requests. A small admin page shows the weights and the last reload, and a dry-run " +
	"endpoint scores a query both ways for support. When the comparison looks right, it removes it."

// registerSearch registers searchPlanMD for acme/app.
func (e *boardEnv) registerSearch() {
	e.t.Helper()
	path := e.write(filepath.Join(e.root, "app", "docs", "search-plan.md"), searchPlanMD)
	e.api("POST", "/api/plans", api.RegisterRequest{Path: path, Repos: []string{"acme/app"}}, http.StatusCreated)
}

// The plan's What cell goes from the parser through the store to the
// package's pane, right under its title: code, bold and italics as such,
// a link as its text, raw HTML as text, a long cell clamped with "more".
// The graph's boxes and the kanban's cards have it as their tooltip.
func TestAppPlanWhat(t *testing.T) {
	e := newPlanEnv(t)
	e.registerSearch()

	page := get(t, e.h, "/app/plans/search-v2?pkg=A1")
	checkNoInline(t, "plan page", page)
	a1 := inner(t, page, "detail")
	wantIn(t, "A1", a1, `<h2>Index builder</h2>
<p class="pd-what">Builds the <code>search</code> index <strong>nightly</strong> from the <em>catalog</em>.</p>`)

	a2 := get(t, e.h, "/app/plans/search-v2?pkg=A2&fragment=detail")
	wantIn(t, "A2", a2, `<p class="pd-what">Answers queries; see the spec. &lt;script&gt;alert(1)&lt;/script&gt; &lt;b&gt;bold?&lt;/b&gt;</p>`)
	wantNotIn(t, "A2", a2, "<script>", `href="https://example.com/spec"`, "<b>")

	a3 := get(t, e.h, "/app/plans/search-v2?pkg=A3&fragment=detail")
	wantIn(t, "A3 clamped", a3, `<details id="pd-what-A3" class="pd-what long"><summary><span class="pd-what-text">Ranks results by`,
		`<code>ranking.toml</code>`, `<span class="more">more</span><span class="less">less</span>`)
	wantNotIn(t, "A1 not clamped", a1, "pd-what long")
	wantNotIn(t, "A4 has none", get(t, e.h, "/app/plans/search-v2?pkg=A4&fragment=detail"), "pd-what")

	// The tooltips: the cell as plain text.
	graph := inner(t, page, "list")
	wantIn(t, "graph tip", itemOf(t, graph, "A1"), "\nBuilds the search index nightly from the catalog.</title>")
	kanban := inner(t, get(t, e.h, "/app/plans/search-v2?view=kanban"), "list")
	wantIn(t, "card tip", itemOf(t, kanban, "A2"), `title="Answers queries; see the spec. &lt;script&gt;alert(1)&lt;/script&gt; &lt;b&gt;bold?&lt;/b&gt;"`)
	if tip := itemOf(t, kanban, "A3"); !strings.Contains(tip, "Ranks results by") || strings.Contains(tip, "removes it.") {
		t.Errorf("A3's card tip isn't cut: %s", tip)
	}
}

func TestWhatSpans(t *testing.T) {
	type sp = mdSpan
	for _, c := range []struct {
		in   string
		want []mdSpan
	}{
		{"", nil},
		{"Plain  text\twith   spaces", []sp{{Text: "Plain text with spaces"}}},
		{"Run `make  check` first", []sp{{Text: "Run "}, {Text: "make  check", Code: true}, {Text: " first"}}},
		{"``a ` b``", []sp{{Text: "a ` b", Code: true}}},
		{"an `unclosed tick", []sp{{Text: "an `unclosed tick"}}},
		{"**bold** and __bold__", []sp{{Text: "bold", Bold: true}, {Text: " and "}, {Text: "bold", Bold: true}}},
		{"*it* and _it_", []sp{{Text: "it", Italic: true}, {Text: " and "}, {Text: "it", Italic: true}}},
		{"**bold *both* bold**", []sp{{Text: "bold ", Bold: true}, {Text: "both", Bold: true, Italic: true}, {Text: " bold", Bold: true}}},
		{"snake_case_name stays", []sp{{Text: "snake_case_name stays"}}},
		{"2 * 3 * 4", []sp{{Text: "2 * 3 * 4"}}},
		{"a lone **star", []sp{{Text: "a lone **star"}}},
		{"***three***", []sp{{Text: "***three***"}}},
		{`\*not italic\*`, []sp{{Text: "*not italic*"}}},
		{"see [docs](https://example.com) and ![a chart](c.png)", []sp{{Text: "see docs and a chart"}}},
		{"[**bold link**](x)", []sp{{Text: "bold link", Bold: true}}},
		{"one<br>two<BR/>three", []sp{{Text: "one"}, {Break: true}, {Text: "two"}, {Break: true}, {Text: "three"}}},
		{"<i>raw</i> & <img src=x>", []sp{{Text: "<i>raw</i> & <img src=x>"}}},
		{"`**not bold**`", []sp{{Text: "**not bold**", Code: true}}},
	} {
		if got := whatSpans(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("whatSpans(%q)\n got %+v\nwant %+v", c.in, got, c.want)
		}
	}
}

func TestPkgWhatLong(t *testing.T) {
	if w := pkgWhatOf("A1", "Short."); w == nil || w.Long || w.ID != "A1" {
		t.Errorf("short = %+v", w)
	}
	if w := pkgWhatOf("A3", longWhat); w == nil || !w.Long {
		t.Errorf("long = %+v", w)
	}
	if w := pkgWhatOf("A5", "one<br>two<br>three<br>four"); w == nil || !w.Long {
		t.Errorf("four lines = %+v", w)
	}
	if w := pkgWhatOf("A4", "  "); w != nil {
		t.Errorf("empty = %+v", w)
	}
	if tip := whatTip("Builds the `search` index **nightly**."); tip != "Builds the search index nightly." {
		t.Errorf("tip = %q", tip)
	}
}
