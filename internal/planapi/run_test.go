package planapi_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// eventually retries cond for up to 5s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// run starts the service's loop for the test.
func (e *env) run() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.svc.Run(ctx)
	}()
	e.t.Cleanup(func() { cancel(); <-done })
}

// A registered plan file is re-parsed when it changes; links and Mark done
// survive, and a file caught empty keeps its packages.
func TestWatchReparses(t *testing.T) {
	e := newEnv(t)
	e.run()
	e.registerSharing()
	ok[api.DoneResult](t, e.req("POST", "/api/plans/sharing/packages/A1/done", nil), http.StatusOK)
	ok[api.LinkResult](t, e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{SessionID: "s", AgentID: "a", Plan: "sharing", Pkg: "V1"}), http.StatusCreated)

	src, err := os.ReadFile(e.sharing)
	must(t, err)
	q4 := "| **Q4 Security review** |"
	i := strings.Index(string(src), q4)
	if i < 0 {
		t.Fatal("fixture has no Q4 row")
	}
	end := i + strings.Index(string(src[i:]), "\n") + 1
	edited := string(src[:end]) + "| **Q5 Release** | Turn the flag on. | Q4 |\n" + string(src[end:])
	e.write(e.sharing, edited)

	eventually(t, "the new package", func() bool { return len(e.board("sharing").Packages) == 16 })
	b := e.board("sharing")
	if p := pkgOf(t, b, "Q5"); p.Title != "Release" || p.Lifecycle != "blocked" || p.BlockedBy[0] != "Q4" {
		t.Errorf("Q5 = %+v", p)
	}
	if p := pkgOf(t, b, "A1"); !p.DoneManual || p.Lifecycle != "done" {
		t.Errorf("A1 lost its Mark done: %+v", p)
	}
	if _, found := findLink(pkgOf(t, b, "V1"), "agent", "s/a"); !found {
		t.Error("V1 lost its spawn link")
	}

	// Caught half-written: an empty file leaves the packages alone.
	e.write(e.sharing, "")
	time.Sleep(1500 * time.Millisecond)
	if n := len(e.board("sharing").Packages); n != 16 {
		t.Errorf("after the file was emptied: %d packages, want 16 kept", n)
	}
	// Editors that save by renaming a new file over the old one.
	tmp := e.sharing + ".tmp"
	e.write(tmp, strings.Replace(edited, "| **Q5 Release** | Turn the flag on. | Q4 |\n", "", 1))
	must(t, os.Rename(tmp, e.sharing))
	eventually(t, "Q5 gone after a rename-save", func() bool { return len(e.board("sharing").Packages) == 15 })
}

// Kick runs a state pass (the daemon calls it after a poll that changed
// something), and a changed board is announced.
func TestKick(t *testing.T) {
	e := newEnv(t)
	e.run()
	e.registerSharing()
	before := e.notes.count(api.TopicPlans)
	must(t, e.st.UpsertPRs(context.Background(), store.PR{Repo: "acme/pivot", Number: 217, HeadBranch: "share/a3-embed-endpoint",
		State: store.PROpen, Title: "Dashboards open in a frame on other sites (A3)"}))
	e.svc.Kick()
	eventually(t, "A3 in review", func() bool { return pkgOf(t, e.board("sharing"), "A3").Lifecycle == "in_review" })
	if e.notes.count(api.TopicPlans) <= before {
		t.Error("no plans change event after the pass")
	}
	// A pass that changes nothing announces nothing.
	n := e.notes.count(api.TopicPlans)
	e.svc.Kick()
	time.Sleep(300 * time.Millisecond)
	if e.notes.count(api.TopicPlans) != n {
		t.Error("an unchanged pass sent a plans change event")
	}
}

// Run picks up a plan file that changed while aimd was down.
func TestRunResyncsAtStart(t *testing.T) {
	e := newEnv(t)
	e.registerSharing()
	src, err := os.ReadFile(e.sharing)
	must(t, err)
	e.write(e.sharing, strings.Replace(string(src), "| **Q4 Security review** |", "| **Q4 Outside audit** |", 1))
	e.run()
	eventually(t, "Q4 renamed", func() bool { return pkgOf(t, e.board("sharing"), "Q4").Title == "Outside audit" })
}
