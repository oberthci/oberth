package main

import (
	"fmt"

	"github.com/oberthci/oberth/internal/store"
)

//nolint:gochecknoglobals // Set by the linker through -X main.version, main.commit and main.date.
var (
	version = "(devel)"
	commit  = "unknown"
	date    = "unknown"
)

func versionFlags() []string { return []string{"--schema"} }
func (a application) versionEntry(args []string) (int, bool) {
	if len(args) == 0 || args[0] != "version" {
		return 0, false
	}
	if len(args) == 2 && args[1] == versionFlags()[0] {
		_, err := fmt.Fprintln(a.out, store.Schema)
		if err != nil {
			return 1, true
		}
		return 0, true
	}
	if len(args) != 1 {
		return a.usage([]string{"version accepts only --schema"}), true
	}

	_, err := fmt.Fprintln(a.out, buildVersion())
	if err != nil {
		return 1, true
	}
	return 0, true
}

func buildVersion() string {
	return fmt.Sprintf("oberth %s commit=%s date=%s", version, commit, date)
}

func binaryVersion() string {
	return version
}
