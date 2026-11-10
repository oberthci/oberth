package main

import (
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/store"
)

// remoteEquivalents names what to run from a workstation instead of an
// in-pod command that reads the server database directly.
var remoteEquivalents = map[string]string{
	"uplink list":   "oberth mcp call uplink_list",
	"uplink rotate": "oberth mcp call uplink_rotate",
	"uplink remove": "oberth mcp call uplink_revoke",
	"access list":   "oberth mcp call access_list",
	"access allow":  "oberth mcp call access_allow",
	"access revoke": "oberth mcp call access_revoke",
	"size get":      "oberth mcp call size_grant_get",
	"size grant":    "oberth mcp call size_grant",
	"size revoke":   "oberth mcp call size_revoke",
	"resource get":  "oberth mcp call resource_grant_get",
	"schedules":     "oberth schedules list",
	"run cancel":    "oberth run cancel <run-id> without --database",
	"runs cancel":   "oberth runs cancel without --database",
}

// inPodCommandError replaces SQLite's "unable to open database file" from a
// command that opens the server database: off the pod there is no database.
type inPodCommandError struct {
	command string
	err     error
}

func (e inPodCommandError) Error() string {
	message := fmt.Sprintf("oberth %s reads the server database, so it runs inside the server pod:\n"+
		"  kubectl exec -n oberth deploy/oberth -- oberth %s ...", e.command, e.command)
	if remote := remoteEquivalents[e.command]; remote != "" {
		message += "\nfrom this machine use: " + remote
	} else if remote := remoteEquivalents[strings.Fields(e.command)[0]]; remote != "" {
		message += "\nfrom this machine use: " + remote
	} else {
		message += "\nit has no remote form"
	}
	return message + fmt.Sprintf("\n(database: %v)", e.err)
}

func (e inPodCommandError) Unwrap() error { return e.err }

// withInPodHint maps a missing server database to inPodCommandError.
func withInPodHint(arguments []string, err error) error {
	if err == nil || !store.IsDatabaseUnavailable(err) || len(arguments) == 0 {
		return err
	}
	command := arguments[0]
	if len(arguments) > 1 && !strings.HasPrefix(arguments[1], "-") {
		command += " " + arguments[1]
	}
	return inPodCommandError{command: command, err: err}
}
