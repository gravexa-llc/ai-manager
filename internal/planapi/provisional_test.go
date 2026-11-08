package planapi_test

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// A spawn whose brief names a package the plan doesn't have makes a
// provisional package, on the board, the summary and the context;
// when the file gains the row, the package is the file's, links and all.
func TestProvisionalSpawn(t *testing.T) {
	e := newEnv(t)
	e.registerSharing()

	res := ok[api.LinkResult](t, e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{
		SessionID: "lead-1", AgentID: "agent-q9", Plan: "sharing", Pkg: "q9", Provisional: true, Title: "Q9: extra QA pass",
	}), http.StatusCreated)
	if len(res.Packages) != 1 {
		t.Fatalf("packages = %+v", res.Packages)
	}
	q9 := res.Packages[0]
	if q9.ID != "Q9" || !q9.Provisional || q9.Title != "Extra QA pass" || q9.FirstSeen.IsZero() {
		t.Errorf("Q9 = %+v", q9)
	}
	// The sharing plan's last package table is "Package | What | Needs".
	if q9.SuggestedRow != "| **Q9 Extra QA pass** |  |  |" {
		t.Errorf("suggested row = %q", q9.SuggestedRow)
	}
	if len(q9.Sources) != 1 || q9.Sources[0].Kind != "spawn" || q9.Sources[0].Ref != "lead-1/agent-q9" || q9.Sources[0].Title != "Extra QA pass" {
		t.Errorf("sources = %+v", q9.Sources)
	}
	if l, found := findLink(q9, "agent", "lead-1/agent-q9"); !found || l.Method != "spawn" || l.Confidence != "exact" {
		t.Errorf("Q9's agent link = %+v (found %v)", l, found)
	}

	b := e.board("sharing")
	if b.Provisional != 1 || b.Total != 16 || slices.Contains(b.ReadyToDispatch, "Q9") {
		t.Errorf("board: provisional %d, total %d, ready %v", b.Provisional, b.Total, b.ReadyToDispatch)
	}
	if last := b.Packages[len(b.Packages)-1]; last.ID != "Q9" {
		t.Errorf("Q9 isn't after the file's packages: %s is last", last.ID)
	}
	sum := ok[api.PlanSummary](t, e.req("GET", "/api/plans/sharing/summary", nil), http.StatusOK)
	if !slices.Equal(ids(sum.Provisional), []string{"Q9"}) || sum.Provisional[0].SuggestedRow != q9.SuggestedRow || !sum.Provisional[0].Provisional {
		t.Errorf("summary provisional = %+v", sum.Provisional)
	}
	c := ok[api.Context](t, e.req("GET", "/api/context?cwd="+e.pivot, nil), http.StatusOK)
	if len(c.Plans) != 1 || c.Plans[0].Path != e.sharing {
		t.Fatalf("context plans = %+v", c.Plans)
	}
	i := slices.IndexFunc(c.Plans[0].Packages, func(p api.ContextPlanPackage) bool { return p.ID == "Q9" })
	if i < 0 || !c.Plans[0].Packages[i].Provisional || c.Plans[0].Packages[i].PlanLine != "Plan: sharing · Q9" {
		t.Errorf("context Q9 = %+v", c.Plans[0].Packages)
	}

	// A second agent for it is another source; the title stays.
	ok[api.LinkResult](t, e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{
		SessionID: "lead-1", AgentID: "agent-q9b", Plan: "sharing", Pkg: "Q9", Provisional: true, Title: "retry",
	}), http.StatusCreated)
	if q9 = pkgOf(t, e.board("sharing"), "Q9"); len(q9.Sources) != 2 || q9.Title != "Extra QA pass" {
		t.Errorf("after a second spawn: title %q, sources %+v", q9.Title, q9.Sources)
	}

	// Provisional on a package the file has: just the link.
	v1 := ok[api.LinkResult](t, e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{
		SessionID: "lead-1", AgentID: "agent-v1", Plan: "sharing", Pkg: "V1", Provisional: true, Title: "email reports",
	}), http.StatusCreated).Packages[0]
	if v1.Provisional || v1.Title != "Email reports" {
		t.Errorf("V1 = %+v", v1)
	}
	refused(t, "provisional, not an ID", e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{SessionID: "s", AgentID: "a", Plan: "sharing", Pkg: "hello", Provisional: true}), http.StatusBadRequest, "package ID")
	refused(t, "provisional, unknown plan", e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{SessionID: "s", AgentID: "a", Plan: "nope", Pkg: "Q9", Provisional: true}), http.StatusNotFound, "nope")

	// The file gains the row: Q9 is the file's, with its agents.
	src, err := os.ReadFile(e.sharing)
	must(t, err)
	q4 := "| **Q4 Security review** |"
	at := strings.Index(string(src), q4)
	if at < 0 {
		t.Fatal("fixture has no Q4 row")
	}
	end := at + strings.Index(string(src[at:]), "\n") + 1
	e.write(e.sharing, string(src[:end])+"| **Q9 Extra QA pass** | More QA. | Q4 |\n"+string(src[end:]))
	reg := ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: []string{e.pivot, "acme/pivot-web"}}), http.StatusOK)
	if reg.Packages != 16 {
		t.Errorf("re-registered with %d packages", reg.Packages)
	}
	b = e.board("sharing")
	q9 = pkgOf(t, b, "Q9")
	if q9.Provisional || q9.SuggestedRow != "" || len(q9.Sources) != 0 || q9.Title != "Extra QA pass" || !slices.Equal(q9.Needs, []string{"Q4"}) {
		t.Errorf("Q9 after its row landed = %+v", q9)
	}
	if _, found := findLink(q9, "agent", "lead-1/agent-q9"); !found {
		t.Errorf("Q9 lost its agent link: %+v", q9.Links)
	}
	if b.Provisional != 0 || b.Total != 16 {
		t.Errorf("board after the merge: provisional %d, total %d", b.Provisional, b.Total)
	}
}
