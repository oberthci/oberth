package argojob

// Existing Workflows may still carry the old in-container observer command.
// Read-only credential and immutable-spec checks inspect its original argv;
// no new Workflow emits or executes this retired server subcommand.
func legacySecurityCommand(command []string) []string {
	if len(command) > 3 && command[0] == "/run/oberth/usage/oberth" && command[1] == "internal-step-exec" && command[2] == "--" {
		return command[3:]
	}
	return command
}
