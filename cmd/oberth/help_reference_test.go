package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Include groups as well as leaves: group help must be useful without opening
// a database, reading credentials or contacting Kubernetes or the server.
var documentedCommands = strings.Split(`access
access allow
access list
access revoke
admin
admin public-identity
admin readiness
artifacts
audit
audit head
audit record
backup
backup database
backup export-secrets
backup key
backup keygen
branch
branch deletions
branch prune
branch prune-refs
branch resolve
fragments
fragments list
fragments show
git-ssh
init
install
issues
issues update
log
events
sync
sync status
mcp
mcp call
mcp tools
client
client setup
client kubectl
client pin
client ports
client repository
client redeem
client renew
outbox
preflight
preflight drift
preflight pki
preflight upstreams
promotion
promotion retry
promotion create
publication
publication abandon
release-wif
release-wif grant
release-wif list
release-wif reset
release-wif revoke
repo
repo add
repo archive
repo remove
repo unarchive
repo verify
repos
restore
run
run cancel
run wait
run hold
run hold abandon
run hold continue
run hold exec
run hold list
run hold patch
run purge
run rerun
run retry
runs
runs cancel
schedules
schedules list
schedules pause
schedules resume
schedules trigger
secretstore
secretstore bootstrap-tls
secretstore exec
secretstore setup
secretstore sync
secretstore verify
serve
setup
status
unseal
upgrade
uplink
uplink add
uplink cert
uplink invite
uplink list
uplink remove
uplink rotate
uplink scope
upstream
upstream add
upstream list
upstream provide-key
upstream remove
validate
version
wait-idle`, "\n")

func TestCommandHelpReference(t *testing.T) {
	for _, key := range []string{"OBERTH_BASE_URL", "OBERTH_MCP_URL", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CA_CERT"} {
		t.Setenv(key, "")
	}
	var reference strings.Builder
	reference.WriteString("# Oberth command reference\n\nHelp rendered from every public command and subcommand. Both `-flag` and\n`--flag` spellings are accepted; `--help` and `-h` return success. Remote\ncommands need the server settings described in [remote CLI](remote-cli.md).\n\n")
	for _, command := range documentedCommands {
		t.Run(command, func(t *testing.T) {
			var out strings.Builder
			args := append(strings.Fields(command), "--help")
			if err := runCLI(t.Context(), args, strings.NewReader(""), &out); err != nil {
				t.Errorf("--help: %v", err)
				return
			}
			help := out.String()
			var short strings.Builder
			if err := runCLI(t.Context(), append(strings.Fields(command), "-h"), strings.NewReader(""), &short); err != nil || short.String() != help {
				t.Errorf("-h differs from --help: %v\n%s", err, &short)
			}
			if strings.TrimSpace(help) == "" || strings.Contains(help, "flag: help requested") {
				t.Errorf("missing command help: %q", help)
				return
			}
			fmt.Fprintf(&reference, "## oberth %s\n\n```text\n%s\n```\n\n", command, referenceHelp(help))
		})
	}

	if t.Failed() {
		return
	}
	rendered := strings.TrimRight(reference.String(), "\n") + "\n"
	path := filepath.Join("..", "..", "docs", "cli-reference.md")
	if os.Getenv("OBERTH_UPDATE_CLI_REFERENCE") == "1" {
		if err := os.WriteFile(path, []byte(rendered), 0644); err != nil {
			t.Fatal(err)
		}
	}
	documented, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(documented) != rendered {
		t.Fatal("documented command help differs from --help; review and regenerate with OBERTH_UPDATE_CLI_REFERENCE=1 go test ./cmd/oberth -run '^TestCommandHelpReference$'")
	}
}

func TestCommandHelpInventory(t *testing.T) {
	// Discover literal flag-set names independently so adding a command requires
	// including its rendered help in the reference. Dynamic action sets and
	// shared parsers are covered by the explicit subcommand inventory above.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "NewFlagSet" || len(call.Args) == 0 {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			name, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(documentedCommands, name) {
				t.Errorf("%s: flag set %q lacks documented --help", path, name)
			}
			return true
		})
	}
	var top strings.Builder
	if err := runCLI(t.Context(), []string{"--help"}, nil, &top); err != nil {
		t.Fatal(err)
	}
	for _, match := range regexp.MustCompile(`(?m)^  ([a-z][a-z-]*) +`).FindAllStringSubmatch(top.String(), -1) {
		if !slices.Contains(documentedCommands, match[1]) {
			t.Errorf("command %q lacks documented --help", match[1])
		}
		if !strings.Contains(usageCommands, match[1]) {
			t.Errorf("usage list omits %q", match[1])
		}
	}
}

func TestHelpIncludesRegisteredFlags(t *testing.T) {
	// Read actual flag registrations independently of help strings and the
	// documentation snapshot, so a handwritten usage cannot silently omit flags.
	dynamic := map[string][]string{
		"parseCancelOptions": {"runs cancel"},
		"parseMCPFlags":      {"mcp call", "mcp tools"},
		"remoteFlags":        {"run", "repos", "status", "issues"},
		"fragmentFlags":      {"fragments list", "fragments show"},
		"runRepoArchive":     {"repo archive", "repo unarchive"},
		"runRemoteSchedules": {"schedules list", "schedules pause", "schedules resume", "schedules trigger"},
	}
	methods := strings.Fields("Bool BoolVar Duration DurationVar Float64 Float64Var Func BoolFunc Int IntVar Int64 Int64Var String StringVar Uint UintVar Uint64 Uint64Var Var")
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			var commands, names []string
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if helper, ok := call.Fun.(*ast.Ident); ok && (helper.Name == "mcpTransportFlags" || helper.Name == "scheduleTransportFlags") {
					names = append(names, "url", "token-file", "ca-file")
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				receiver, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				index := 0
				switch {
				case receiver.Name == "flag" && selector.Sel.Name == "NewFlagSet":
					if literal, ok := call.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						value, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatal(err)
						}
						commands = append(commands, value)
					} else {
						commands = append(commands, dynamic[function.Name.Name]...)
					}
					return true
				case receiver.Name == "flags" && slices.Contains(methods, selector.Sel.Name):
					if strings.HasSuffix(selector.Sel.Name, "Var") || selector.Sel.Name == "Var" {
						index = 1
					}
				default:
					return true
				}
				if len(call.Args) > index {
					if literal, ok := call.Args[index].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						value, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatal(err)
						}
						names = append(names, value)
					}
				}
				return true
			})
			if variants, ok := dynamic[function.Name.Name]; ok {
				commands = variants
			}
			for _, command := range commands {
				var out strings.Builder
				err = runCLI(t.Context(), append(strings.Fields(command), "--help"), strings.NewReader(""), &out)
				if err != nil {
					t.Errorf("%s --help: %v", command, err)
					continue
				}
				for _, name := range names {
					pattern := regexp.MustCompile(`(^|[\s\[])--?` + regexp.QuoteMeta(name) + `([\s=\]]|$)`)
					if !pattern.MatchString(out.String()) {
						t.Errorf("%s: %s --help omits registered flag --%s", path, command, name)
					}
				}
			}
		}
	}
}

func TestGitSSHHelpNeedsNoTransport(t *testing.T) {
	t.Setenv("OBERTH_GIT_SSH_CONFIG", "")
	for _, arg := range []string{"--help", "-h"} {
		var out, diagnostics strings.Builder
		if code := runGitSSH(t.Context(), []string{arg}, nil, &out, &diagnostics); code != 0 || out.String() != gitSSHHelp || diagnostics.Len() != 0 {
			t.Fatalf("git-ssh help: %d %q %q", code, &out, &diagnostics)
		}
	}
}

// Terminal help uses tabs and occasionally blank descriptions. Preserve its
// text while giving Markdown consistent indentation and no trailing spaces.
func referenceHelp(help string) string {
	lines := strings.Split(strings.TrimSpace(help), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(strings.ReplaceAll(line, "\t", "    "), " ")
	}
	return strings.Join(lines, "\n")
}
