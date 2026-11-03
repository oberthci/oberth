package argoworkflow

// Weighted Dilworth: replace each weighted vertex by incomparable unit copies.
// A capacitated bipartite matching computes their chain cover without expanding
// bytes of memory into individual vertices. Max antichain = total - max flow.
func maxWeightedAntichain(before [][]bool, weights []int64) int64 {
	n := len(weights)
	source, sink := 2*n, 2*n+1
	type edge struct {
		to, reverse int
		capacity    int64
	}
	graph := make([][]edge, 2*n+2)
	add := func(a, b int, c int64) {
		graph[a] = append(graph[a], edge{b, len(graph[b]), c})
		graph[b] = append(graph[b], edge{a, len(graph[a]) - 1, 0})
	}
	var total int64
	for _, w := range weights {
		total += w
	}
	for i, w := range weights {
		add(source, i, w)
		add(n+i, sink, w)
		for j := range weights {
			if before[i][j] {
				add(i, n+j, total+1)
			}
		}
	}
	var flow int64
	for {
		level := make([]int, len(graph))
		for i := range level {
			level[i] = -1
		}
		level[source] = 0
		queue := []int{source}
		for len(queue) > 0 {
			a := queue[0]
			queue = queue[1:]
			for _, e := range graph[a] {
				if e.capacity > 0 && level[e.to] < 0 {
					level[e.to] = level[a] + 1
					queue = append(queue, e.to)
				}
			}
		}
		if level[sink] < 0 {
			return total - flow
		}
		next := make([]int, len(graph))
		var send func(int, int64) int64
		send = func(a int, limit int64) int64 {
			if a == sink {
				return limit
			}
			for next[a] < len(graph[a]) {
				i := next[a]
				e := graph[a][i]
				if e.capacity > 0 && level[e.to] == level[a]+1 {
					sent := send(e.to, min(limit, e.capacity))
					if sent > 0 {
						graph[a][i].capacity -= sent
						graph[e.to][e.reverse].capacity += sent
						return sent
					}
				}
				next[a]++
			}
			return 0
		}
		for {
			sent := send(source, total)
			if sent == 0 {
				break
			}
			flow += sent
		}
	}
}
