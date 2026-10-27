//go:build !linux

package stepusage

import "io"

// Run is an internal Linux-container entrypoint, not a host CLI command.
func Run(_ []string, _ io.Reader, _, stderr io.Writer) int {
	_, _ = io.WriteString(stderr, "step-exec requires Linux\n")
	return 126
}
