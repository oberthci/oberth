package sshserver

import (
	"net/url"
	"strings"
)

func pushWatchURLs(base string, handler UpdateHandler) []string {
	results, ok := handler.(interface{ QueuedRunIDs() []string })
	if !ok {
		return []string{base}
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return []string{base}
	}
	targets := []string{}
	seen := map[string]bool{}
	for _, id := range results.QueuedRunIDs() {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		target := *parsed
		prefix := strings.TrimSuffix(strings.TrimRight(target.Path, "/"), "/runs")
		target.Path = prefix + "/runs/" + id
		target.RawPath = ""
		targets = append(targets, target.String())
	}
	if len(targets) == 0 {
		return []string{base}
	}
	return targets
}
