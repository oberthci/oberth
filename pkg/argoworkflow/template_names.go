package argoworkflow

import (
	"fmt"
)

func (g *graph) missingLocal(c *call, scope string) {
	f := finding(c.path, "local_template")
	closest, distance := "", int(^uint(0)>>1)
	if len(c.template) > 63 {
		g.findings = append(g.findings, f)
		return
	}
	for _, n := range g.scopes[scope].nodes {
		if len(n.template.Name) > 63 {
			continue
		}
		if !g.constructing(c.path) {
			return
		}
		if d := nameDistance(c.template, n.template.Name); d < distance {
			closest, distance = n.template.Name, d
		}
	}
	if closest != "" {
		f.Remedy = fmt.Sprintf("Correct the template name; closest existing template: %q.", closest)
	}
	// Long native names remain in detail; the remedy is bounded for every name.
	if len(f.Remedy) > 120 {
		g.details = append(g.details, "closest existing template: "+closest)
		f.Remedy = messages()["local_template"].remedy
	}
	g.findings = append(g.findings, f)
}

func nameDistance(a, b string) int {
	row := make([]int, len(b)+1)
	for i := range row {
		row[i] = i
	}
	for i := range len(a) {
		diagonal := row[0]
		row[0] = i + 1
		for j := range len(b) {
			previous := row[j+1]
			cost := 0
			if a[i] != b[j] {
				cost = 1
			}
			row[j+1] = min(row[j]+1, previous+1, diagonal+cost)
			diagonal = previous
		}
	}
	return row[len(b)]
}
