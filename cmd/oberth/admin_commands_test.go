package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

// TestPrintedCommandsAreExactCommandLines parses every command a refusal or
// finding can print against the command catalogue: the path exists, each
// flag is one the command accepts with its value, and the positional
// arguments fit the command's bounds.
func TestPrintedCommandsAreExactCommandLines(t *testing.T) {
	grant := publicerr.Grant{Repo: "origin/team/widget", File: ".oberth/build.yaml", Step: "build", Path: "registry/key"}
	identity := grant
	identity.Path, identity.Audience = "identity", "sts.example"
	catalogue := commandCatalogue()
	for _, line := range []string{
		publicerr.UnarchiveCommand("origin/team/widget"),
		publicerr.PromoteCommand("origin/team/widget", strings.Repeat("a", 40)),
		publicerr.IssueDeleteCommand(42),
		publicerr.ReceiveRetryCommand("origin/team/widget"),
		publicerr.RepoAddCommand("ssh://git@forge/team/widget.git", ""),
		publicerr.RepoAddCommand("ssh://git@forge/team/widget.git", "origin"),
		publicerr.RepoAddTrustCommand("ssh://git@forge/team/widget.git", "known_hosts", "origin", false),
		publicerr.RepoAddTrustCommand("ssh://git@forge/team/widget.git", "known_hosts", "origin", true),
		publicerr.CheckCommand("origin/team/widget", ".oberth/build.yaml"),
		publicerr.SecretStoreCheckCommand(),
		publicerr.AccessAllowCommand("", grant),
		publicerr.AccessAllowCommand("configmap", grant),
		publicerr.AccessAllowCommand("identity", identity),
		publicerr.ClaimAllowCommand("origin/team/widget", "build-cache", false),
		publicerr.SecretPutCommand(grant),
		publicerr.CapabilityGrantCommand("origin/team/widget", `{"namespace":"ci","service_account":"it's"}`),
	} {
		if problem := commandLineProblem(catalogue, line); problem != "" {
			t.Errorf("%s: %s", line, problem)
		}
	}
	if commandLineProblem(catalogue, "oberth repo unarchive 'a' --unknown 'b'") == "" || commandLineProblem(catalogue, "oberth repo revive 'a'") == "" || commandLineProblem(catalogue, "oberth repo unarchive") == "" {
		t.Fatal("the catalogue check accepts commands the CLI refuses")
	}
}

func commandLineProblem(catalogue map[string]commandSyntax, line string) string {
	words, ok := shellWords(line)
	if !ok || len(words) < 2 || words[0] != "oberth" {
		return "not one oberth command line"
	}
	syntax, words, found := commandPath(catalogue, words[1:])
	if !found {
		return "unknown command path"
	}
	positional := 0
	for i := 0; i < len(words); i++ {
		if words[i] == "<" {
			break
		}
		if !strings.HasPrefix(words[i], "--") {
			positional++
			continue
		}
		arity, known := syntax.Flags[words[i]]
		if !known {
			return "unknown flag " + words[i]
		}
		i += arity
	}
	if positional < syntax.Args[0] || positional > syntax.Args[1] {
		return "positional arguments outside the command's bounds"
	}
	return ""
}

// commandPath finds the longest leading words that name a catalogued command
// and returns its syntax with the words after the path.
func commandPath(catalogue map[string]commandSyntax, words []string) (commandSyntax, []string, bool) {
	for n := len(words); n > 0; n-- {
		if row, found := catalogue[strings.Join(words[:n], " ")]; found {
			return row, words[n:], true
		}
	}
	return commandSyntax{}, nil, false
}

// shellWords splits a line the way a POSIX shell splits plain, single-quoted
// and backslash-escaped words, which is all the printed commands use.
func shellWords(line string) ([]string, bool) {
	var words []string
	var word strings.Builder
	started, quoted, escaped := false, false, false
	for _, r := range line {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case r == '\\' && !quoted:
			escaped, started = true, true
		case r == '\'':
			quoted, started = !quoted, true
		case r == ' ' && !quoted:
			if started {
				words, started = append(words, word.String()), false
				word.Reset()
			}
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if started {
		words = append(words, word.String())
	}
	return words, !quoted && !escaped
}
