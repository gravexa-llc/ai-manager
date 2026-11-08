package web

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fixturePackages parses a plan fixture into a board's packages.
func fixturePackages(t *testing.T, name string) []api.Package {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "plans", name))
	must(t, err)
	var out []api.Package
	for _, p := range plans.Parse(src).Packages {
		out = append(out, api.Package{ID: p.ID, Title: p.Title, Needs: p.Needs, Lifecycle: "ready"})
	}
	return out
}

// edgeSet names a layout's edges "From→To".
func edgeSet(g graphLayout) []string {
	var out []string
	for _, e := range g.Edges {
		out = append(out, g.Nodes[e.From].ID+"→"+g.Nodes[e.To].ID)
	}
	slices.Sort(out)
	return out
}

// checkLayout checks what holds for every layout: each package placed
// once, in its own slot; the edges exactly the Needs between known
// packages; every forward edge going right, through one placeholder per
// column it crosses; longest-path columns.
func checkLayout(t *testing.T, name string, pkgs []api.Package, g graphLayout) {
	t.Helper()
	if len(g.Nodes) != len(pkgs) {
		t.Fatalf("%s: %d nodes for %d packages", name, len(g.Nodes), len(pkgs))
	}
	known := map[string]bool{}
	for i, p := range pkgs {
		if g.Nodes[i].ID != p.ID {
			t.Errorf("%s: node %d is %s, want %s", name, i, g.Nodes[i].ID, p.ID)
		}
		known[strings.ToUpper(p.ID)] = true
	}
	slots := map[[2]int]string{}
	place := func(what string, layer, order int) {
		if layer < 0 || layer >= g.Layers || order < 0 || order >= g.Rows {
			t.Errorf("%s: %s at (%d, %d) is outside %d×%d", name, what, layer, order, g.Layers, g.Rows)
		}
		k := [2]int{layer, order}
		if slots[k] != "" {
			t.Errorf("%s: %s and %s share slot %v", name, what, slots[k], k)
		}
		slots[k] = what
	}
	for _, nd := range g.Nodes {
		place(nd.ID, nd.Layer, nd.Order)
	}
	for i, d := range g.dummies {
		place(fmt.Sprintf("placeholder %d", i), d.Layer, d.Order)
	}

	var want []string
	for _, p := range pkgs {
		for _, n := range p.Needs {
			if known[strings.ToUpper(n)] && !strings.EqualFold(n, p.ID) {
				want = append(want, strings.ToUpper(n)+"→"+p.ID)
			}
		}
	}
	slices.Sort(want)
	want = slices.Compact(want)
	if got := edgeSet(g); !slices.Equal(got, want) {
		t.Errorf("%s: edges\n got %v\nwant %v", name, got, want)
	}

	deps := map[int][]int{}
	for _, e := range g.Edges {
		from, to := g.Nodes[e.From], g.Nodes[e.To]
		if e.Back {
			if len(e.via) != 0 || from.Layer < to.Layer {
				t.Errorf("%s: back edge %s→%s goes right or has placeholders", name, from.ID, to.ID)
			}
			continue
		}
		deps[e.To] = append(deps[e.To], e.From)
		if from.Layer >= to.Layer {
			t.Errorf("%s: edge %s→%s goes from column %d to %d", name, from.ID, to.ID, from.Layer, to.Layer)
		}
		if len(e.via) != to.Layer-from.Layer-1 {
			t.Errorf("%s: edge %s→%s spans %d columns with %d placeholders", name, from.ID, to.ID, to.Layer-from.Layer, len(e.via))
		}
		for k, d := range e.via {
			if g.dummies[d].Layer != from.Layer+1+k {
				t.Errorf("%s: edge %s→%s placeholder %d in column %d", name, from.ID, to.ID, k, g.dummies[d].Layer)
			}
		}
	}
	for i, nd := range g.Nodes {
		want := 0
		for _, d := range deps[i] {
			want = max(want, g.Nodes[d].Layer+1)
		}
		if nd.Layer != want {
			t.Errorf("%s: %s in column %d, want %d (longest path)", name, nd.ID, nd.Layer, want)
		}
	}
}

func TestLayoutFixtures(t *testing.T) {
	for _, c := range []struct {
		file          string
		packages      int
		layersAtLeast int
	}{
		{"sharing-plan.md", 15, 3},
		{"ROADMAP.md", 26, 5},
	} {
		pkgs := fixturePackages(t, c.file)
		if len(pkgs) != c.packages {
			t.Fatalf("%s has %d packages, want %d", c.file, len(pkgs), c.packages)
		}
		g := layoutGraph(pkgs)
		checkLayout(t, c.file, pkgs, g)
		if g.Layers < c.layersAtLeast {
			t.Errorf("%s: %d columns, want at least %d", c.file, g.Layers, c.layersAtLeast)
		}
		for _, nd := range g.Nodes {
			if nd.InCycle {
				t.Errorf("%s: %s flagged as in a cycle", c.file, nd.ID)
			}
		}
		for _, e := range g.Edges {
			if e.Back || e.Cycle {
				t.Errorf("%s: edge %+v flagged as a cycle's", c.file, e)
			}
		}
		// The sweeps never end with more crossings than plan order.
		plain := layoutGraphUnswept(pkgs)
		if g.Crossings > plain {
			t.Errorf("%s: %d crossings after the sweeps, %d before", c.file, g.Crossings, plain)
		}
		t.Logf("%s: %d columns, %d rows, %d edges, %d placeholders, %d crossings (%d in plan order)",
			c.file, g.Layers, g.Rows, len(g.Edges), len(g.dummies), g.Crossings, plain)
	}

	// A few edges the fixtures are known for.
	aim := edgeSet(layoutGraph(fixturePackages(t, "ROADMAP.md")))
	for _, e := range []string{"F1→A1", "S1→A1", "B2→B3", "C3→C6", "B3→C6", "D1→D3", "C6→D3", "D2→E1"} {
		if !slices.Contains(aim, e) {
			t.Errorf("ROADMAP.md: no edge %s", e)
		}
	}
	sharing := edgeSet(layoutGraph(fixturePackages(t, "sharing-plan.md")))
	for _, e := range []string{"A1→A2", "W1→A3", "A1→T1", "G1→T1"} {
		if !slices.Contains(sharing, e) {
			t.Errorf("sharing: no edge %s", e)
		}
	}
}

// layoutGraphUnswept counts the crossings of the starting order, before
// any sweep.
func layoutGraphUnswept(pkgs []api.Package) int {
	return layoutGraphSweeps(pkgs, 0).Crossings
}

// Provisional packages that no edge joins go in a last column of
// their own, in plan order; with no package of the file, they are the
// first column.
func TestLayoutProvisional(t *testing.T) {
	pkgs := []api.Package{
		{ID: "A1"}, {ID: "A2", Needs: []string{"A1"}}, {ID: "B1"},
		{ID: "W12", Provisional: true}, {ID: "C8", Provisional: true},
	}
	g := layoutGraph(pkgs)
	layer, order := map[string]int{}, map[string]int{}
	for _, n := range g.Nodes {
		layer[n.ID], order[n.ID] = n.Layer, n.Order
	}
	if g.Layers != 3 || layer["A1"] != 0 || layer["B1"] != 0 || layer["A2"] != 1 || layer["W12"] != 2 || layer["C8"] != 2 {
		t.Errorf("layers %d: %v", g.Layers, layer)
	}
	if order["W12"] != 0 || order["C8"] != 1 {
		t.Errorf("the provisional column's order: %v", order)
	}
	g = layoutGraph([]api.Package{{ID: "W12", Provisional: true}})
	if g.Layers != 1 || g.Nodes[0].Layer != 0 {
		t.Errorf("only provisional: %+v", g)
	}
}

// A dependency cycle is laid out (one edge drawn against the flow) and
// flagged; a package needing itself and an unknown ID draw no edge.
func TestLayoutCycle(t *testing.T) {
	pkgs := []api.Package{
		{ID: "A1", Needs: []string{"C1"}},
		{ID: "B1", Needs: []string{"A1"}},
		{ID: "C1", Needs: []string{"b1"}},
		{ID: "D1", Needs: []string{"A1", "A1"}},
		{ID: "E1", Needs: []string{"E1"}},
		{ID: "F1", Needs: []string{"Z9", "D1"}},
	}
	g := layoutGraph(pkgs)
	checkLayout(t, "cycle", pkgs, g)
	for _, nd := range g.Nodes {
		if want := nd.ID == "A1" || nd.ID == "B1" || nd.ID == "C1"; nd.InCycle != want {
			t.Errorf("%s InCycle = %v", nd.ID, nd.InCycle)
		}
	}
	back := 0
	for _, e := range g.Edges {
		from, to := g.Nodes[e.From].ID, g.Nodes[e.To].ID
		inCycle := from != "A1" || to != "D1"
		if to == "F1" {
			inCycle = false
		}
		if e.Cycle != inCycle {
			t.Errorf("edge %s→%s Cycle = %v", from, to, e.Cycle)
		}
		if e.Back {
			back++
		}
	}
	if back != 1 {
		t.Errorf("%d back edges, want 1 to break the one cycle", back)
	}
	s := drawGraph(g, planGraphMetrics, "g", "label", func(id string) string { return "#" + id })
	var cyc int
	for _, e := range s.Edges {
		if strings.Contains(e.Class, "cycle") {
			cyc++
			if e.Marker != "c" {
				t.Errorf("cycle edge %+v has the plain arrowhead", e)
			}
		}
	}
	if cyc != 3 {
		t.Errorf("%d cycle edges drawn, want 3", cyc)
	}
	for _, n := range s.Nodes {
		if n.X < 0 || n.Y < 0 || n.X+n.W > s.W || n.Y+n.H > s.H {
			t.Errorf("box %s at (%d,%d) is outside the %d×%d drawing", n.ID, n.X, n.Y, s.W, s.H)
		}
	}
	if len(layoutGraph(nil).Nodes) != 0 {
		t.Error("an empty plan has nodes")
	}
}
