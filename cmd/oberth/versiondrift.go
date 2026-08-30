package main

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/mod/semver"
)

// warnVersionDrift says so when this binary is older than the server it just
// talked to.
//
// Only that direction. A newer CLI against an older server is the ordinary
// state during a rollout and needs no comment, while an older CLI is the one
// that sends a request the server understands differently, or fails to send a
// flag that exists now -- and it reads as the server misbehaving.
func warnVersionDrift(output io.Writer, cliVersion, serverVersion string) {
	cli := canonicalRelease(cliVersion)
	server := canonicalRelease(serverVersion)
	// A development build has no place on this scale, and neither has a
	// version string that is not a release.
	if cli == "" || server == "" || semver.Compare(cli, server) >= 0 {
		return
	}
	_, _ = fmt.Fprintf(output,
		"\nWarning: this CLI is %s and the server is %s. Upgrade the CLI before trusting what it reports.\n",
		strings.TrimSpace(cliVersion), strings.TrimSpace(serverVersion))
}

// canonicalRelease turns "0.18.0" or "v0.18.0" into a comparable semver, and
// everything else into the empty string.
func canonicalRelease(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "v") {
		raw = "v" + raw
	}
	if !semver.IsValid(raw) {
		return ""
	}
	return raw
}
