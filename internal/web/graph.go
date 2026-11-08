package web

// The dependency graph: a plan's Needs laid out and drawn as an inline SVG,
// which the app's plan page shows in its graph view (app_plans.go).
//
// The SVG is drawn here, on the server: no script, no library, nothing
// fetched. The layout is a plain layered one (Sugiyama-style):
//
//  1. Edges run from a dependency to the package that needs it. Each
//     dependency cycle (a strongly connected component of two or more
//     packages) is flagged; the edges a depth-first search finds going
//     back up its path are left out of the layering, and drawn as arcs
//     against the flow.
//  2. Longest-path layering: a package with no dependency is in column 0,
//     every other one a column right of its furthest dependency. A
//     provisional package that no edge joins goes in a last column
//     of its own, right of the plan's.
//  3. An edge that spans columns gets a placeholder in each column it
//     crosses, so it is routed around the boxes there.
//  4. A few barycenter sweeps (down by predecessors, up by successors)
//     reorder each column to cut crossings, keeping the best order found;
//     then neighbours in a column swap places while that removes more.
//
// The page's CSP (default-src 'self') blocks inline style attributes, so
// the SVG uses classes (app-plan.css: colours by lifecycle) and SVG
// presentation attributes (coordinates, path data) only. Every text in it
// goes through html/template like the rest of the page.

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// graphNode is one package in a layout.
type graphNode struct {
	ID, Title, Lifecycle string
	// Layer is its column; Order its row within the column.
	Layer, Order int
	// InCycle is true when it is in a dependency cycle.
	InCycle bool
}

// graphEdge runs from a dependency (From) to the package that needs it
// (To), as indexes into graphLayout.Nodes.
type graphEdge struct {
	From, To int
	// Cycle is true when both ends are in the same dependency cycle; Back
	// when the layering left it out to break that cycle (it is drawn
	// against the flow).
	Cycle, Back bool
	// via are the placeholders it passes through, one per column it
	// crosses, as indexes into graphLayout.dummies.
	via []int
}

// graphDummy is an edge's placeholder in a column it crosses.
type graphDummy struct{ Layer, Order int }

// graphLayout is a plan's dependency graph, laid out in columns.
type graphLayout struct {
	Nodes  []graphNode // in plan order
	Edges  []graphEdge // by dependent, then dependency, in plan order
	Layers int
	// Rows is the most nodes and placeholders in one column.
	Rows    int
	dummies []graphDummy
	// Crossings counts the edge crossings between adjacent columns of the
	// order chosen.
	Crossings int
}

// barycenterSweeps is how many times the columns are reordered;
// transposePasses bounds the neighbour swaps after them.
const (
	barycenterSweeps = 8
	transposePasses  = 6
)

// layoutGraph lays out the dependency graph of a plan's packages. Needs
// that name no package of the plan, and a package needing itself, draw no
// edge (they are parser warnings).
func layoutGraph(pkgs []api.Package) graphLayout {
	return layoutGraphSweeps(pkgs, barycenterSweeps)
}

// layoutGraphSweeps is layoutGraph with that many barycenter sweeps.
func layoutGraphSweeps(pkgs []api.Package, sweeps int) graphLayout {
	n := len(pkgs)
	g := graphLayout{Nodes: make([]graphNode, n)}
	idx := make(map[string]int, n)
	for i, p := range pkgs {
		g.Nodes[i] = graphNode{ID: p.ID, Title: p.Title, Lifecycle: p.Lifecycle}
		if _, dup := idx[strings.ToUpper(p.ID)]; !dup {
			idx[strings.ToUpper(p.ID)] = i
		}
	}
	out := make([][]int, n) // edge indexes by From
	seen := map[[2]int]bool{}
	for j, p := range pkgs {
		for _, need := range p.Needs {
			i, ok := idx[strings.ToUpper(strings.TrimSpace(need))]
			if !ok || i == j || seen[[2]int{i, j}] {
				continue
			}
			seen[[2]int{i, j}] = true
			out[i] = append(out[i], len(g.Edges))
			g.Edges = append(g.Edges, graphEdge{From: i, To: j})
		}
	}

	// 1. Cycles: strongly connected components, then back edges.
	comp := components(n, g.Edges, out)
	size := map[int]int{}
	for _, c := range comp {
		size[c]++
	}
	for i := range g.Nodes {
		g.Nodes[i].InCycle = size[comp[i]] > 1
	}
	for k := range g.Edges {
		e := &g.Edges[k]
		e.Cycle = comp[e.From] == comp[e.To] && size[comp[e.From]] > 1
	}
	state := make([]int, n) // 0 new, 1 on the path, 2 done
	var visit func(v int)
	visit = func(v int) {
		state[v] = 1
		for _, k := range out[v] {
			switch w := g.Edges[k].To; state[w] {
			case 0:
				visit(w)
			case 1:
				g.Edges[k].Back = true
			}
		}
		state[v] = 2
	}
	for v := range n {
		if state[v] == 0 {
			visit(v)
		}
	}

	// 2. Longest-path layering over the forward edges, in topological
	// order (Kahn's, lowest plan index first).
	indeg := make([]int, n)
	for _, e := range g.Edges {
		if !e.Back {
			indeg[e.To]++
		}
	}
	done := make([]bool, n)
	for range n {
		v := -1
		for i := range n {
			if !done[i] && indeg[i] == 0 {
				v = i
				break
			}
		}
		if v < 0 {
			break // can't happen: back edges broke every cycle
		}
		done[v] = true
		for _, k := range out[v] {
			if e := g.Edges[k]; !e.Back {
				g.Nodes[e.To].Layer = max(g.Nodes[e.To].Layer, g.Nodes[v].Layer+1)
				indeg[e.To]--
			}
		}
	}
	// Provisional packages that nothing joins go in a column of
	// their own, after the plan's: they aren't in the plan file yet.
	lastReal, loose := -1, make([]bool, n)
	joined := make([]bool, n)
	for _, e := range g.Edges {
		joined[e.From], joined[e.To] = true, true
	}
	for i, p := range pkgs {
		if p.Provisional && !joined[i] {
			loose[i] = true
		} else {
			lastReal = max(lastReal, g.Nodes[i].Layer)
		}
	}
	for i := range g.Nodes {
		if loose[i] {
			g.Nodes[i].Layer = lastReal + 1
		}
	}
	for _, nd := range g.Nodes {
		g.Layers = max(g.Layers, nd.Layer+1)
	}

	// 3. Placeholders, and the vertex graph the ordering works on:
	// vertices 0..n-1 are the nodes, n+d the placeholder d.
	layerOf := func(v int) int {
		if v < n {
			return g.Nodes[v].Layer
		}
		return g.dummies[v-n].Layer
	}
	var preds, succs [][]int
	grow := func(v int) {
		for len(preds) <= v {
			preds, succs = append(preds, nil), append(succs, nil)
		}
	}
	join := func(a, b int) {
		grow(max(a, b))
		succs[a] = append(succs[a], b)
		preds[b] = append(preds[b], a)
	}
	grow(n - 1)
	var dummyTo []int // the node each placeholder's edge leads to
	for k := range g.Edges {
		e := &g.Edges[k]
		if e.Back {
			continue
		}
		prev := e.From
		for l := g.Nodes[e.From].Layer + 1; l < g.Nodes[e.To].Layer; l++ {
			g.dummies = append(g.dummies, graphDummy{Layer: l})
			dummyTo = append(dummyTo, e.To)
			d := len(g.dummies) - 1
			e.via = append(e.via, d)
			join(prev, n+d)
			prev = n + d
		}
		join(prev, e.To)
	}
	layers := make([][]int, g.Layers)
	// Start in plan order; a placeholder sits by the package its edge
	// leads to.
	key := func(v int) float64 {
		if v < n {
			return float64(v)
		}
		return float64(dummyTo[v-n]) - 0.5
	}
	for v := range n + len(g.dummies) {
		layers[layerOf(v)] = append(layers[layerOf(v)], v)
	}
	for _, l := range layers {
		slices.SortStableFunc(l, func(a, b int) int { return cmp.Compare(key(a), key(b)) })
	}

	// 4. Barycenter sweeps.
	pos := make([]int, n+len(g.dummies))
	place := func() {
		for _, l := range layers {
			for i, v := range l {
				pos[v] = i
			}
		}
	}
	crossings := func() int {
		c := 0
		for _, l := range layers {
			var segs [][2]int
			for _, u := range l {
				for _, w := range succs[u] {
					segs = append(segs, [2]int{pos[u], pos[w]})
				}
			}
			for i := range segs {
				for j := i + 1; j < len(segs); j++ {
					if (segs[i][0]-segs[j][0])*(segs[i][1]-segs[j][1]) < 0 {
						c++
					}
				}
			}
		}
		return c
	}
	reorder := func(l []int, nbrs [][]int) {
		bary := make(map[int]float64, len(l))
		for _, v := range l {
			if len(nbrs[v]) == 0 {
				bary[v] = float64(pos[v])
				continue
			}
			sum := 0
			for _, u := range nbrs[v] {
				sum += pos[u]
			}
			bary[v] = float64(sum) / float64(len(nbrs[v]))
		}
		slices.SortStableFunc(l, func(a, b int) int { return cmp.Compare(bary[a], bary[b]) })
		for i, v := range l {
			pos[v] = i
		}
	}
	place()
	best, bestOrder := crossings(), clone2(layers)
	for sweep := range sweeps {
		if sweep%2 == 0 {
			for l := 1; l < g.Layers; l++ {
				reorder(layers[l], preds)
			}
		} else {
			for l := g.Layers - 2; l >= 0; l-- {
				reorder(layers[l], succs)
			}
		}
		if c := crossings(); c < best {
			best, bestOrder = c, clone2(layers)
		}
	}
	layers = bestOrder
	place()
	// Then swap neighbours in a column while that removes crossings.
	for pass := 0; pass < transposePasses && sweeps > 0; pass++ {
		improved := false
		for _, l := range layers {
			for i := 0; i+1 < len(l); i++ {
				l[i], l[i+1] = l[i+1], l[i]
				pos[l[i]], pos[l[i+1]] = i, i+1
				if c := crossings(); c < best {
					best, improved = c, true
					continue
				}
				l[i], l[i+1] = l[i+1], l[i]
				pos[l[i]], pos[l[i+1]] = i, i+1
			}
		}
		if !improved {
			break
		}
	}
	g.Crossings = best
	for v := range n + len(g.dummies) {
		if v < n {
			g.Nodes[v].Order = pos[v]
		} else {
			g.dummies[v-n].Order = pos[v]
		}
	}
	for _, l := range layers {
		g.Rows = max(g.Rows, len(l))
	}
	return g
}

func clone2(ls [][]int) [][]int {
	out := make([][]int, len(ls))
	for i, l := range ls {
		out[i] = slices.Clone(l)
	}
	return out
}

// components returns each node's strongly connected component (Tarjan's
// algorithm).
func components(n int, edges []graphEdge, out [][]int) []int {
	index, low, comp := make([]int, n), make([]int, n), make([]int, n)
	onStack := make([]bool, n)
	for i := range index {
		index[i], comp[i] = -1, -1
	}
	var stack []int
	next, ncomp := 0, 0
	var visit func(v int)
	visit = func(v int) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, k := range out[v] {
			w := edges[k].To
			if index[w] < 0 {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp[w] = ncomp
				if w == v {
					break
				}
			}
			ncomp++
		}
	}
	for v := range n {
		if index[v] < 0 {
			visit(v)
		}
	}
	return comp
}

// ---- Drawing ----

// graphMetrics sizes a drawing, in SVG units (pixels at 100%).
type graphMetrics struct {
	nodeW, nodeH, gapX, gapY, pad int
	// titles puts each package's title under its ID.
	titles bool
	// titleRunes is the longest title shown whole.
	titleRunes int
}

// backArc is the room above the boxes for edges drawn against the flow.
const backArc = 26

// svgNode is a package's box.
type svgNode struct {
	ID, Title, Lifecycle string
	Href, Tip            string
	Cycle                bool
	X, Y, W, H           int
	// TX, TY place the ID; TY2 the title.
	TX, TY, TY2 int
}

// svgEdge is an edge's path.
type svgEdge struct {
	D, Class string
	// Marker names its arrowhead: "a", or "c" for a cycle's.
	Marker string
	// From and To are the IDs of the package needed and the package that
	// needs it, for a drawing that highlights a package's edges (the app's
	// plan page).
	From, To string
}

// svgGraph is a drawing, which the plan page puts in its SVG (app_plans.go).
type svgGraph struct {
	// ID prefixes the drawing's element ids (its title, its arrowheads),
	// so two drawings on one page don't clash.
	ID    string
	Label string
	Mini  bool
	W, H  int
	Nodes []svgNode
	Edges []svgEdge
}

// drawGraph places a layout's boxes and routes its edges. href gives a
// package's link (on the plan page, its selection).
func drawGraph(g graphLayout, m graphMetrics, id, label string, href func(id string) string) svgGraph {
	top := 0
	for _, e := range g.Edges {
		if e.Back {
			top = backArc
		}
	}
	slot := m.nodeH + m.gapY
	rows := make([]int, g.Layers) // how many in each column
	for _, nd := range g.Nodes {
		rows[nd.Layer]++
	}
	for _, d := range g.dummies {
		rows[d.Layer]++
	}
	x := func(layer int) int { return m.pad + layer*(m.nodeW+m.gapX) }
	y := func(layer, order int) int {
		return m.pad + top + (g.Rows-rows[layer])*slot/2 + order*slot
	}
	s := svgGraph{ID: id, Label: label, Mini: !m.titles}
	if g.Layers > 0 {
		s.W = 2*m.pad + g.Layers*m.nodeW + (g.Layers-1)*m.gapX
		s.H = 2*m.pad + top + g.Rows*slot - m.gapY
	}
	for _, nd := range g.Nodes {
		v := svgNode{ID: nd.ID, Lifecycle: nd.Lifecycle, Href: href(nd.ID), Cycle: nd.InCycle,
			X: x(nd.Layer), Y: y(nd.Layer, nd.Order), W: m.nodeW, H: m.nodeH}
		v.Tip = nd.ID + " " + nd.Title + ": " + strings.ToLower(lifecycleLabel(nd.Lifecycle))
		if nd.InCycle {
			v.Tip += " (in a dependency cycle)"
		}
		if m.titles {
			v.Title = clip(nd.Title, m.titleRunes)
			v.TX, v.TY, v.TY2 = v.X+8, v.Y+17, v.Y+34
		} else {
			v.TX, v.TY = v.X+m.nodeW/2, v.Y+m.nodeH/2+4
		}
		s.Nodes = append(s.Nodes, v)
	}
	mid := func(l, o int) int { return y(l, o) + m.nodeH/2 }
	for _, e := range g.Edges {
		from, to := g.Nodes[e.From], g.Nodes[e.To]
		ed := svgEdge{Class: "g-edge", Marker: "a", From: from.ID, To: to.ID}
		if from.Lifecycle == "done" {
			ed.Class += " met"
		}
		if e.Cycle {
			ed.Class += " cycle"
			ed.Marker = "c"
		}
		var d strings.Builder
		if e.Back {
			// From the top of the dependency, up and back over the
			// columns, down into the top of the package that needs it.
			x1, y1 := x(from.Layer)+m.nodeW/2, y(from.Layer, from.Order)
			x2, y2 := x(to.Layer)+m.nodeW/2, y(to.Layer, to.Order)
			peak := max(2, min(y1, y2)-backArc)
			fmt.Fprintf(&d, "M%d %d C%d %d %d %d %d %d", x1, y1, x1, peak, x2, peak, x2, y2)
		} else {
			px, py := x(from.Layer)+m.nodeW, mid(from.Layer, from.Order)
			fmt.Fprintf(&d, "M%d %d", px, py)
			curve := func(qx, qy int) {
				mx := (px + qx) / 2
				fmt.Fprintf(&d, " C%d %d %d %d %d %d", mx, py, mx, qy, qx, qy)
				px, py = qx, qy
			}
			for _, k := range e.via {
				dm := g.dummies[k]
				curve(x(dm.Layer), mid(dm.Layer, dm.Order))
				px = x(dm.Layer) + m.nodeW
				fmt.Fprintf(&d, " L%d %d", px, py)
			}
			curve(x(to.Layer), mid(to.Layer, to.Order))
		}
		ed.D = d.String()
		s.Edges = append(s.Edges, ed)
	}
	return s
}

// clip shortens s to at most n runes, with an ellipsis.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
