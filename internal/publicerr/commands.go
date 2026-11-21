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
//
// A value that begins with "-" would be read by oberth as a flag after the
// shell removes the quotes, and could select another grant. Such a value is
// printed attached to its own flag (--step='-x'), before every positional
// word; a value that has no flag form makes the constructor return "", so no
// command is printed.

// AdministratorLine is how every view prints an administrator command.
func AdministratorLine(command string) string { return "Administrator: " + command }

// WithCommand records the complete command an administrator runs to resolve
// this refusal. Views print it as its own line after the context, so reason
// and remedy keep their bounds. An empty command records none.
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
	return newLine("repo unarchive").arg(repo).String()
}

// PromoteCommand promotes an exact tested commit as an administrator.
func PromoteCommand(repo, sha string) string {
	return newLine("promote").arg(sha).flag("repo", repo).String()
}

// IssueDeleteCommand deletes one issue; issue IDs start at 1.
func IssueDeleteCommand(id int64) string {
	if id < 1 {
		return ""
	}
	return newLine("issue delete").word(strconv.FormatInt(id, 10)).String()
}

// ReceiveRetryCommand retries the recorded push delivery of one repository.
func ReceiveRetryCommand(repo string) string {
	return newLine("receive outbox retry").flag("repo", repo).String()
}

// RepoAddCommand registers a forge repository, optionally under an upstream.
func RepoAddCommand(address, upstream string) string {
	c := newLine("repo add").arg(address)
	if upstream != "" {
		c.flag("upstream", upstream)
	}
	return c.String()
}

// RepoAddTrustCommand registers a forge repository after its host key is
// verified; repin replaces a changed key.
func RepoAddTrustCommand(address, knownHosts, upstream string, repin bool) string {
	c := newLine("repo add").arg(address).flag("known-hosts", knownHosts).word("--expected-host-fingerprint", "VERIFIED_FINGERPRINT").flag("upstream", upstream)
	if repin {
		c.word("--repin")
	}
	return c.String()
}

// CheckCommand inspects a local pipeline file against a repository.
func CheckCommand(repo, file string) string {
	return newLine("check").flag("repo", repo).flag("local", file).String()
}

// SecretStoreCheckCommand checks the configured secret store.
func SecretStoreCheckCommand() string {
	return newLine("secretstore check").String()
}

// Grant names one exact access declaration: the repository, file, step and
// path, with the projected token audience for an identity grant.
type Grant struct{ Repo, File, Step, Path, Audience string }

// AccessAllowCommand approves a grant of the given kind; the empty kind is
// the secret store path grant.
func AccessAllowCommand(kind string, g Grant) string {
	c := newLine("access allow")
	if kind != "" {
		c.word("--kind", kind)
	}
	if g.Audience != "" {
		c.flag("audience", g.Audience)
	}
	return c.selectors([2]string{"repo", g.Repo}, [2]string{"workflow", g.File}, [2]string{"step", g.Step}, [2]string{"secret", g.Path}).String()
}

// ClaimAllowCommand approves a persistent volume claim for the CI or release tier.
func ClaimAllowCommand(repo, claim string, release bool) string {
	tier := "ci"
	if release {
		tier = "release"
	}
	return newLine("access allow").word("--kind", "pvc", "--tier", tier).selectors([2]string{"repo", repo}, [2]string{"claim", claim}).String()
}

// SecretPutCommand stores a granted secret; the value is read from standard
// input and never appears in the command.
func SecretPutCommand(g Grant) string {
	return newLine("secret put").flag("repo", g.Repo).flag("file", g.File).flag("step", g.Step).arg(g.Path).word("<", "<value-file>").String()
}

// CapabilityGrantCommand grants a workflow capability given as its JSON text.
func CapabilityGrantCommand(repo, capability string) string {
	return newLine("workflow capability grant").flag("repo", repo).flag("capability", capability).flag("reason", "<reason>").String()
}

// SourcePinCommand lets the consumer's pipelines pin commits of the producer
// its own pipeline names. The pair is one consumer=producer value, so a name
// that begins with "-", holds "=" or is empty prints no command. A release
// version keeps the upgrade on that version; "" leaves the version out.
func SourcePinCommand(consumer, producer, version string) string {
	for _, name := range []string{consumer, producer} {
		if name == "" || flagLike(name) || strings.Contains(name, "=") {
			return ""
		}
	}
	c := newLine("upgrade")
	if version != "" {
		c.flag("version", version)
	}
	return c.flag("source-pin", consumer+"="+producer).String()
}

// commandLine collects the words of one command in order; unsafe marks a
// value that would be read as a flag and has no flag form.
type commandLine struct {
	words  []string
	unsafe bool
}

func newLine(path string) *commandLine { return &commandLine{words: []string{"oberth", path}} }

// word adds fixed words this file writes: flag names, modes and placeholders.
func (c *commandLine) word(words ...string) *commandLine {
	c.words = append(c.words, words...)
	return c
}

// flag adds a named value; one that begins with "-" is attached to the flag,
// so no argument reader takes it for a flag of its own.
func (c *commandLine) flag(name, value string) *commandLine {
	if flagLike(value) {
		return c.word("--" + name + "=" + quote(value))
	}
	return c.word("--"+name, quote(value))
}

// arg adds a positional value that has no flag form.
func (c *commandLine) arg(value string) *commandLine {
	c.unsafe = c.unsafe || flagLike(value)
	return c.word(quote(value))
}

// selectors adds positional names that also have flag forms, in the order the
// command reads them: each one that begins with "-" is given as its flag
// before the rest, which stay positional; the reader skips a name already set.
func (c *commandLine) selectors(pairs ...[2]string) *commandLine {
	for _, p := range pairs {
		if flagLike(p[1]) {
			c.flag(p[0], p[1])
		}
	}
	for _, p := range pairs {
		if !flagLike(p[1]) {
			c.word(quote(p[1]))
		}
	}
	return c
}

func (c *commandLine) String() string {
	if c.unsafe {
		return ""
	}
	return strings.Join(c.words, " ")
}

func flagLike(value string) bool { return strings.HasPrefix(value, "-") }

func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
