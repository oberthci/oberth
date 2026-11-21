package main

import (
	"encoding/json"
	"errors"
	"flag"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

// readPrinted pastes a printed command into the real argument readers: the
// shell's word splitting, the common options, then the reader of that command
// (access allow, an installation command, another flow, or a tool command).
// It returns what each argument was read as: flag names, "arg<n>" for flow
// positionals, and the tool's own field names.
func readPrinted(t *testing.T, line string) (map[string]string, error) {
	t.Helper()
	words, ok := shellWords(line)
	if !ok || len(words) < 2 || words[0] != "oberth" {
		t.Fatalf("not one oberth command line: %q", line)
	}
	if at := slices.Index(words, "<"); at >= 0 {
		words = words[:at]
	}
	_, rest, problems := parseCommon(words[1:])
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	if len(rest) >= 2 && rest[0] == "access" && rest[1] == "allow" {
		request, err := grantRequest(rest[2:])
		return jsonFields(request), err
	}
	if fields, found, err := readFlowCommand(rest); found {
		return fields, err
	}
	return readToolCommand(t, line, rest)
}

// readFlowCommand reads an installation or other flow command, if rest names one.
func readFlowCommand(rest []string) (map[string]string, bool, error) {
	named := func(name string) bool {
		n := len(strings.Fields(name))
		return len(rest) >= n && strings.Join(rest[:n], " ") == name
	}
	for _, row := range installationCommands() {
		if named(row.name) {
			f := installationFlags(row.name)
			args, err := installationArgs(row, f, rest[len(strings.Fields(row.name)):])
			if err == nil && (len(args) < row.minArgs || len(args) > row.maxArgs) {
				err = errors.New("positional arguments outside the command's bounds")
			}
			return flowFields(f, args), true, err
		}
	}
	for _, flow := range flowCommands() {
		if named(flow.name) {
			f := flowFlags(flow.name, "")
			args, err := parseFlow(f, rest[len(strings.Fields(flow.name)):])
			return flowFields(f, args), true, err
		}
	}
	return nil, false, nil
}

func readToolCommand(t *testing.T, line string, rest []string) (map[string]string, error) {
	t.Helper()
	row, args, found := findCommand(commands(), rest)
	if !found {
		t.Fatalf("no command reads %q", line)
	}
	data, problems := toolArgs(row, args, nil, nil)
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	return rawFields(raw), nil
}

func flowFields(f *flag.FlagSet, args []string) map[string]string {
	fields := map[string]string{}
	f.Visit(func(entry *flag.Flag) { fields[entry.Name] = entry.Value.String() })
	for i, arg := range args {
		fields["arg"+strconv.Itoa(i)] = arg
	}
	return fields
}

func jsonFields(value any) map[string]string {
	data, _ := json.Marshal(value)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	return rawFields(raw)
}

func rawFields(raw map[string]json.RawMessage) map[string]string {
	fields := map[string]string{}
	for name, value := range raw {
		var text string
		if json.Unmarshal(value, &text) != nil {
			text = string(value)
		}
		fields[name] = text
	}
	return fields
}

// TestReviewedFlagNamesReadBackAsTheIntendedGrant pastes the four reviewed
// names into the printed grant command: each is read back as exactly the
// grant printed. The same names in the earlier all-positional form are
// refused instead of read as a wider grant.
func TestReviewedFlagNamesReadBackAsTheIntendedGrant(t *testing.T) {
	repo := "origin/team/widget"
	for _, tc := range []struct{ file, step, path string }{
		{".oberth/build.yaml", "--step=*", "registry/key"},
		{".oberth/build.yaml", "--secret=prod/deploy-key", "registry/key"},
		{".oberth/build.yaml", "--repo=origin/other/thing", "registry/key"},
		{"--kind", "pvc", "claim"},
	} {
		want := map[string]string{"repo": repo, "workflow": tc.file, "step": tc.step, "secret": tc.path}
		line := publicerr.AccessAllowCommand("", publicerr.Grant{Repo: repo, File: tc.file, Step: tc.step, Path: tc.path})
		got, err := readPrinted(t, line)
		if err != nil || !maps.Equal(got, want) {
			t.Errorf("%s\nread as %v %v, want %v", line, got, err, want)
		}
		positional := "oberth access allow " + strings.Join([]string{shellArgument(repo), shellArgument(tc.file), shellArgument(tc.step), shellArgument(tc.path)}, " ")
		if got, err := readPrinted(t, positional); err == nil {
			t.Errorf("%s\nwas read as the grant %v instead of refused", positional, got)
		}
	}
	for _, line := range []string{
		"oberth secret put --repo 'r' --file 'f' --step 'build' '--step=*' < <value-file>",
		"oberth access allow 'origin/team/widget' --workflow '.oberth/build.yaml' 'build' 'registry/key'",
		"oberth access allow 'origin/team/widget' '.oberth/build.yaml' 'build' '--step=*'",
		"oberth access allow 'origin/team/widget' '--kind=pvc' '--tier=ci' 'claim'",
		"oberth access allow --step 'a' --step 'b' 'origin/team/widget' '.oberth/build.yaml' 'registry/key'",
	} {
		if got, err := readPrinted(t, line); err == nil {
			t.Errorf("%s\nwas read as %v instead of refused", line, got)
		}
	}
}

type printedCommand struct {
	name     string
	normal   []string
	build    func(a []string) string
	want     func(a []string) map[string]string
	noFlagAt []int // argument positions with no flag form
}

func printedCommands() []printedCommand {
	grant := func(a []string) publicerr.Grant {
		return publicerr.Grant{Repo: a[0], File: a[1], Step: a[2], Path: a[3]}
	}
	selected := func(a []string) map[string]string {
		return map[string]string{"repo": a[0], "workflow": a[1], "step": a[2], "secret": a[3]}
	}
	return []printedCommand{
		{"repo unarchive", []string{"origin/team/widget"}, func(a []string) string { return publicerr.UnarchiveCommand(a[0]) },
			func(a []string) map[string]string { return map[string]string{"repo": a[0]} }, []int{0}},
		{"promote", []string{"origin/team/widget", strings.Repeat("a", 40)}, func(a []string) string { return publicerr.PromoteCommand(a[0], a[1]) },
			func(a []string) map[string]string { return map[string]string{"repo": a[0], "arg0": a[1]} }, []int{1}},
		{"issue delete", nil, func([]string) string { return publicerr.IssueDeleteCommand(42) },
			func([]string) map[string]string { return map[string]string{"id": "42"} }, nil},
		{"receive outbox retry", []string{"origin/team/widget"}, func(a []string) string { return publicerr.ReceiveRetryCommand(a[0]) },
			func(a []string) map[string]string { return map[string]string{"action": "retry", "repo": a[0]} }, nil},
		{"repo add", []string{"ssh://git@forge/team/widget.git", "origin"}, func(a []string) string { return publicerr.RepoAddCommand(a[0], a[1]) },
			func(a []string) map[string]string { return map[string]string{"arg0": a[0], "upstream": a[1]} }, []int{0}},
		{"repo add trust", []string{"ssh://git@forge/team/widget.git", "known_hosts", "origin"}, func(a []string) string { return publicerr.RepoAddTrustCommand(a[0], a[1], a[2], true) },
			func(a []string) map[string]string {
				return map[string]string{"arg0": a[0], "known-hosts": a[1], "expected-host-fingerprint": "VERIFIED_FINGERPRINT", "upstream": a[2], "repin": "true"}
			}, []int{0}},
		{"check", []string{"origin/team/widget", ".oberth/build.yaml"}, func(a []string) string { return publicerr.CheckCommand(a[0], a[1]) },
			func(a []string) map[string]string { return map[string]string{"repo": a[0], "local": a[1]} }, nil},
		{"secretstore check", nil, func([]string) string { return publicerr.SecretStoreCheckCommand() },
			func([]string) map[string]string { return map[string]string{} }, nil},
		{"access allow", []string{"origin/team/widget", ".oberth/build.yaml", "build", "registry/key"}, func(a []string) string { return publicerr.AccessAllowCommand("", grant(a)) },
			selected, nil},
		{"access allow identity", []string{"origin/team/widget", ".oberth/build.yaml", "build", "sts.example"}, func(a []string) string {
			return publicerr.AccessAllowCommand("identity", publicerr.Grant{Repo: a[0], File: a[1], Step: a[2], Path: "identity", Audience: a[3]})
		}, func(a []string) map[string]string {
			return map[string]string{"kind": "identity", "audience": a[3], "repo": a[0], "workflow": a[1], "step": a[2], "secret": "identity"}
		}, nil},
		{"access allow pvc", []string{"origin/team/widget", "build-cache"}, func(a []string) string { return publicerr.ClaimAllowCommand(a[0], a[1], true) },
			func(a []string) map[string]string {
				return map[string]string{"kind": "pvc", "tier": "release", "repo": a[0], "claim": a[1]}
			}, nil},
		{"secret put", []string{"origin/team/widget", ".oberth/build.yaml", "build", "registry/key"}, func(a []string) string { return publicerr.SecretPutCommand(grant(a)) },
			func(a []string) map[string]string {
				return map[string]string{"repo": a[0], "file": a[1], "step": a[2], "arg0": a[3]}
			}, []int{3}},
		{"upgrade source pin", []string{"origin/team/widget", "origin/team/source"}, func(a []string) string { return publicerr.SourcePinCommand(a[0], a[1], "") },
			func(a []string) map[string]string { return map[string]string{"source-pin": a[0] + "=" + a[1]} }, []int{0, 1}},
		{"upgrade source pin at version", []string{"origin/team/widget", "origin/team/source", "v0.30.3"}, func(a []string) string { return publicerr.SourcePinCommand(a[0], a[1], a[2]) },
			func(a []string) map[string]string {
				return map[string]string{"source-pin": a[0] + "=" + a[1], "version": a[2]}
			}, []int{0, 1}},
		{"workflow capability grant", []string{"origin/team/widget", "ci"}, func(a []string) string { return publicerr.CapabilityGrantCommand(a[0], namespaceJSON(a[1])) },
			func(a []string) map[string]string {
				return map[string]string{"repo": a[0], "capability": namespaceJSON(a[1]), "reason": "<reason>"}
			}, nil},
	}
}

// namespaceJSON is a capability text naming one namespace: the capability
// value is JSON, so a hostile name reaches it inside the object.
func namespaceJSON(name string) string {
	text, _ := json.Marshal(name)
	return `{"namespace":` + string(text) + `}`
}

// TestEveryPrintedCommandReadsBackItsOwnNames puts hostile names in every
// position of every command the builder prints and pastes the result into
// the real readers: what is read equals what was meant. Only a name with no
// flag form leaves no command at all.
func TestEveryPrintedCommandReadsBackItsOwnNames(t *testing.T) {
	hostile := []string{"--step=*", "--secret=prod/deploy-key", "--repo=origin/other/thing", "--kind", "pvc", "-x", "--json", "--help", "--detail", "-o", "--url=https://other", "--token-file=/tmp/t", "--reason=r", "--", "it's", "a b", "*"}
	for _, c := range printedCommands() {
		cases := [][]string{c.normal}
		for at := range c.normal {
			for _, value := range hostile {
				args := slices.Clone(c.normal)
				args[at] = value
				cases = append(cases, args)
			}
		}
		for _, args := range cases {
			line := c.build(args)
			if line == "" {
				if !omittable(c, args) {
					t.Errorf("%s %q: no command although every name has a flag form", c.name, args)
				}
				continue
			}
			got, err := readPrinted(t, line)
			if err != nil || !maps.Equal(got, c.want(args)) {
				t.Errorf("%s\nread as %v %v, want %v", line, got, err, c.want(args))
			}
		}
	}
}

func omittable(c printedCommand, args []string) bool {
	for _, at := range c.noFlagAt {
		if strings.HasPrefix(args[at], "-") {
			return true
		}
	}
	return false
}
