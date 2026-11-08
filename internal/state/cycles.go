package state

import (
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// cycles finds the dependency cycles longer than one among a plan's
// packages: every strongly connected component of the Needs graph with two
// or more packages. It returns each member's cycle as a path through the
// component that starts and ends with its first member in plan order
// (["A1", "B1", "A1"]), keyed by canonID, and the cycles themselves in plan
// order. A package needing itself is the parser's warning, not a cycle
// here.
func cycles(pkgs []store.Package) (map[string][]string, [][]string) {
	idx := map[string]int{}
	for i, p := range pkgs {
		idx[canonID(p.PkgID)] = i
	}
	adj := make([][]int, len(pkgs))
	for i, p := range pkgs {
		for _, n := range p.Needs {
			if j, ok := idx[canonID(n)]; ok && j != i {
				adj[i] = append(adj[i], j)
			}
		}
	}

	// Tarjan's algorithm.
	index := make([]int, len(pkgs))
	low := make([]int, len(pkgs))
	onStack := make([]bool, len(pkgs))
	for i := range index {
		index[i] = -1
	}
	var stack []int
	var comps [][]int
	next := 0
	var visit func(v int)
	visit = func(v int) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, u := range adj[v] {
			switch {
			case index[u] < 0:
				visit(u)
				low[v] = min(low[v], low[u])
			case onStack[u]:
				low[v] = min(low[v], index[u])
			}
		}
		if low[v] == index[v] {
			var comp []int
			for {
				u := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[u] = false
				comp = append(comp, u)
				if u == v {
					break
				}
			}
			if len(comp) > 1 {
				comps = append(comps, comp)
			}
		}
	}
	for v := range pkgs {
		if index[v] < 0 {
			visit(v)
		}
	}

	byPkg := map[string][]string{}
	var all [][]string
	for _, comp := range comps {
		in := map[int]bool{}
		start := comp[0]
		for _, v := range comp {
			in[v] = true
			start = min(start, v)
		}
		path := cyclePath(adj, in, start)
		ids := make([]string, len(path))
		for i, v := range path {
			ids[i] = pkgs[v].PkgID
		}
		all = append(all, ids)
		for v := range in {
			byPkg[canonID(pkgs[v].PkgID)] = ids
		}
	}
	// Plan order: by each cycle's first member.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && idx[canonID(all[j][0])] < idx[canonID(all[j-1][0])]; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	return byPkg, all
}

// cyclePath returns a shortest path from start back to start inside the
// component, by breadth-first search, as [start, …, start].
func cyclePath(adj [][]int, in map[int]bool, start int) []int {
	prev := map[int]int{}
	queue := []int{start}
	seen := map[int]bool{}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for _, u := range adj[v] {
			if !in[u] {
				continue
			}
			if u == start {
				path := []int{start}
				for x := v; x != start; x = prev[x] {
					path = append(path, x)
				}
				path = append(path, start)
				// path is start, …, start backwards: reverse the middle.
				for i, j := 1, len(path)-2; i < j; i, j = i+1, j-1 {
					path[i], path[j] = path[j], path[i]
				}
				return path
			}
			if !seen[u] {
				seen[u] = true
				prev[u] = v
				queue = append(queue, u)
			}
		}
	}
	return []int{start, start} // unreachable for a real component
}
