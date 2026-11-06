package main

import "strings"

func nearestCommand(input string) string {
	commands := strings.Fields("access admin admission artifacts audit backup fragments git-ssh init install issues log mcp preflight promotion publication repo repos resource restore run runs schedules secretstore serve setup size status uplink upgrade upstream validate version wait-idle")
	best, distance := "", 3
	for _, command := range commands {
		candidate := commandDistance(input, command)
		if candidate < distance {
			best, distance = command, candidate
		}
	}
	if len(input) < 3 {
		return ""
	}
	return best
}

func commandDistance(a, b string) int {
	// Command names are ASCII; a small edit-distance table also handles typos.
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		next := make([]int, len(b)+1)
		next[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			next[j] = min(next[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		previous = next
	}
	return previous[len(b)]
}
