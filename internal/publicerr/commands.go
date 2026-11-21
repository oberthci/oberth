package publicerr

import (
	"slices"
	"strconv"
	"strings"
)

// The oberth commands that refusals and findings print are built here and
// nowhere else. Each is complete: the command path, then every value as one
// single-quoted shell word, so it pastes as written. A constructor takes only
// the refused object's own public names, never a secret, a token or another
// repository's name; a value the administrator chooses is a <placeholder>.

// AdministratorLine is how every view prints an administrator command.
func AdministratorLine(command string) string { return "Administrator: " + command }

// WithCommand records the complete command an administrator runs to resolve
// this refusal. Views print it as its own line after the context, so reason
// and remedy keep their bounds.
func (e *Error) WithCommand(command string) *Error {
	e.command = command
	return e
}

// Command returns the administrator command, or "" when there is none.
func (e *Error) Command() string {
	if e == nil {
		return ""
	}
	return e.command
}

func (e *Error) publicContext() []string {
	if e == nil {
		return nil
	}
	if e.command == "" {
		return e.Context
	}
	return append(slices.Clip(e.Context), AdministratorLine(e.command))
}

// classifiedCommand keeps the one command the refusals of the selected code
// share; when they name different commands, each becomes a context line.
func classifiedCommand(refusals []refusal, code Code, contexts []string) (string, []string) {
	var commands []string
	for _, entry := range refusals {
		if entry.err.Code() == code && entry.err.Command() != "" {
			commands = append(commands, entry.err.Command())
		}
	}
	slices.Sort(commands)
	commands = slices.Compact(commands)
	if len(commands) == 1 {
		return commands[0], contexts
	}
	for _, command := range commands {
		contexts = append(contexts, AdministratorLine(command))
	}
	return "", contexts
}

// UnarchiveCommand reactivates an archived repository.
func UnarchiveCommand(repo string) string {
	return line("repo unarchive", quote(repo))
}

// PromoteCommand promotes an exact tested commit as an administrator.
func PromoteCommand(repo, sha string) string {
	return line("promote", quote(sha), "--repo", quote(repo))
}

// IssueDeleteCommand deletes one issue.
func IssueDeleteCommand(id int64) string {
	return line("issue delete", strconv.FormatInt(id, 10))
}

// ReceiveRetryCommand retries the recorded push delivery of one repository.
func ReceiveRetryCommand(repo string) string {
	return line("receive outbox retry", "--repo", quote(repo))
}

// RepoAddCommand registers a forge repository, optionally under an upstream.
func RepoAddCommand(address, upstream string) string {
	if upstream == "" {
		return line("repo add", quote(address))
	}
	return line("repo add", quote(address), "--upstream", quote(upstream))
}

// RepoAddTrustCommand registers a forge repository after its host key is
// verified; repin replaces a changed key.
func RepoAddTrustCommand(address, knownHosts, upstream string, repin bool) string {
	words := []string{quote(address), "--known-hosts", quote(knownHosts), "--expected-host-fingerprint", "VERIFIED_FINGERPRINT", "--upstream", quote(upstream)}
	if repin {
		words = append(words, "--repin")
	}
	return line("repo add", words...)
}

// CheckCommand inspects a local pipeline file against a repository.
func CheckCommand(repo, file string) string {
	return line("check", "--repo", quote(repo), "--local", quote(file))
}

// SecretStoreCheckCommand checks the configured secret store.
func SecretStoreCheckCommand() string {
	return line("secretstore check")
}

// Grant names one exact access declaration: the repository, file, step and
// path, with the projected token audience for an identity grant.
type Grant struct{ Repo, File, Step, Path, Audience string }

// AccessAllowCommand approves a grant of the given kind; the empty kind is
// the secret store path grant.
func AccessAllowCommand(kind string, g Grant) string {
	var words []string
	if kind != "" {
		words = append(words, "--kind", kind)
	}
	if g.Audience != "" {
		words = append(words, "--audience", quote(g.Audience))
	}
	words = append(words, quote(g.Repo), quote(g.File), quote(g.Step), quote(g.Path))
	return line("access allow", words...)
}

// ClaimAllowCommand approves a persistent volume claim for the CI or release tier.
func ClaimAllowCommand(repo, claim string, release bool) string {
	tier := "ci"
	if release {
		tier = "release"
	}
	return line("access allow", "--kind", "pvc", "--tier", tier, quote(repo), quote(claim))
}

// SecretPutCommand stores a granted secret; the value is read from standard
// input and never appears in the command.
func SecretPutCommand(g Grant) string {
	return line("secret put", "--repo", quote(g.Repo), "--file", quote(g.File), "--step", quote(g.Step), quote(g.Path), "<", "<value-file>")
}

// CapabilityGrantCommand grants a workflow capability given as its JSON text.
func CapabilityGrantCommand(repo, capability string) string {
	return line("workflow capability grant", "--repo", quote(repo), "--capability", quote(capability), "--reason", quote("<reason>"))
}

func line(path string, words ...string) string {
	return strings.Join(append([]string{"oberth", path}, words...), " ")
}

func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
