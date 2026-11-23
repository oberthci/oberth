package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/publicerr"
)

type flowSession struct {
	app      application
	ctx      context.Context
	options  commonOptions
	remote   *client.Client
	resolved client.Options
	last     json.RawMessage
}
type flowCommand struct {
	name, usage, summary string
	minArgs, maxArgs     int
	run                  func(*flowSession, []string) error
}

func flowCommands() []flowCommand {
	commands := []flowCommand{
		{"issue import", "issue import <file> [--dry-run] [--upstream <local>]", "Import JSON Lines without overwriting records: 1048576 bytes per file and encoded request; 131072 bytes per line and complete issue. Use several files with disjoint numbers, in file order, into an installation with no issues, before its first push. Format and example: docs/issue-import.md.", 1, 1, importIssues},
		{"access allow", "access allow <repo> <file> <step> <path> | --kind pvc <repo> <claim>", "Approve the exact access recipe printed by admission.", 0, 4, allowAccess},
		{"repo add", "repo add <forge URL> [--known-hosts file] [--expected-host-fingerprint fingerprint] [--repin] [--upstream name]", "Register a forge repository and bind this checkout.", 1, 1, addRepository},
		{"invite", "invite <upstream> [--identity name] [--out file] [--expires duration]", "Invite an uplink to every repository in one upstream.", 1, 1, invite},
		{"join", "join <invitation-file>", "Join an installation and bind this checkout.", 1, 1, join},
		{"renew", "renew", "Renew the selected profile certificate with its retained key.", 0, 0, renew},
		{"promote", "promote [sha] [--repo repo]", "Promote a tested commit and follow its CI run.", 0, 1, promote}, {"release", "release <tag>", "Check, tag, push and follow a release.", 1, 1, release},
		{"audit", "audit", "Read complete ordered audit history.", 0, 0, audit}, {"log", "log <run> [step] [--pattern expression]", "Follow retained logs for a run.", 1, 2, logRun},
		{"init", "init [--release] [--force]", "Write a starter pipeline for review.", 0, 0, initialize}, {"validate", "validate [file]", "Validate an authored pipeline file.", 0, 1, validate},
		{"check", "check [ref] [--local file] [--repo repo]", "Inspect a repository revision or local file.", 0, 1, check},
	}
	return commands
}

func (a application) flow(ctx context.Context, args []string) (int, bool) {
	options, rest, problems := parseCommon(args)
	for _, command := range flowCommands() {
		words := strings.Fields(command.name)
		if len(rest) < len(words) || strings.Join(rest[:len(words)], " ") != command.name {
			continue
		}
		if row, _, ok := findCommand(commands(), rest); ok && len(strings.Fields(row.path)) > len(words) {
			return 0, false
		}
		if len(rest) == len(words)+1 && rest[len(words)] == "-" {
			return 0, false
		}

		if len(problems) > 0 {
			return a.usage(problems), true
		}
		return a.executeFlow(ctx, command, flowRequest{options, rest[len(words):]}), true
	}
	return 0, false
}
func (s *flowSession) close() {
	if s.remote != nil {
		s.remote.Close()
	}
}
func (s *flowSession) connect() error {
	if s.remote != nil {
		return nil
	}
	resolved, err := client.Resolve(s.options.transport, s.app.base, s.app.env)
	if err != nil {
		return err
	}
	cfg := s.app.config
	cfg.Options = resolved
	cfg.Diagnostic = s.app.diagnostic
	s.remote, err = client.New(cfg)
	s.resolved = resolved
	return err
}
func (s *flowSession) invoke(name string, args, out any) error {
	if err := s.connect(); err != nil {
		return err
	}
	row, _, ok := findCommand(commands(), []string{"tool", name})
	var paths map[string]string
	if ok {
		var err error
		paths, err = client.ReserveCredentials(s.app.base, row.definition, s.resolved)
		if err != nil {
			return err
		}
		defer client.CleanupCredentials(paths)
		if row.definition.LongPoll {
			if err = writeWrapped(s.app.diagnostic, waitingForArgs(row.definition, args)); err != nil {
				return err
			}
		}
	}
	var raw json.RawMessage
	if err := s.remote.Invoke(s.ctx, name, args, &raw); err != nil {
		return err
	}
	public, err := client.SaveCredentials(raw, paths)
	if err != nil {
		return err
	}
	s.last = public
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}
func (s *flowSession) fact(format string, args ...any) error {
	if s.options.json {
		return nil
	}
	return writeWrapped(s.app.out, fmt.Sprintf(format, args...))
}
func (s *flowSession) failed(err error) int {
	var p usageProblems
	if errors.As(err, &p) {
		return s.app.usage(p)
	}
	return s.app.failedDetail(err, s.options.detail)
}

type usageProblems []string

func (p usageProblems) Error() string { return "invalid command arguments" }
func parseFlow(f *flag.FlagSet, args []string) ([]string, error) {
	f.SetOutput(io.Discard)
	flags, positionals, err := splitFlowArgs(f, args)
	if err != nil {
		return nil, err
	}
	if err = f.Parse(flags); err != nil {
		return nil, usageProblems{"Invalid flags; use help for this command."}
	}
	for _, row := range flowCommands() {
		if f.Name() == row.name && !validFlowArgs(row.name, positionals) {
			return nil, usageProblems{"Use oberth " + row.usage}
		}
	}
	return positionals, nil
}

// splitFlowArgs separates flags from positional arguments in any order and
// names the first flag the command does not define.
func splitFlowArgs(f *flag.FlagSet, args []string) (flags, positionals []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
		name, _, equal := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		entry := f.Lookup(name)
		if entry == nil {
			if name != "" && name != "h" && name != "help" {
				return nil, nil, usageProblems{"Unknown flag " + commandWord("--"+name) + "; use help for this command."}
			}
			continue
		}
		boolean, ok := entry.Value.(interface{ IsBoolFlag() bool })
		if equal || ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positionals, nil
}
func flowError(key string, cause error) *publicerr.Error {
	m := flowMessages()[key]
	return publicerr.New(publicerr.Unavailable, m[0], m[1], cause)
}

// serverTrustRemedy names the flag and the file that holds the Oberth server's
// SSH host keys, which is not the forge's.
const serverTrustRemedy = "Pass --known-hosts with the SSH trust file that install printed."

func flowMessages() map[string][2]string {
	return map[string][2]string{
		"invitation_file":    {"Invitation file is not valid JSON.", "Ask the administrator for a new invitation file."},
		"invite_trust":       {"The profile's SSH trust does not pin this installation.", "Run oberth invite with the administrator profile that install saved."},
		"invitation_remove":  {"Joined, but could not remove the spent invitation file.", "Remove the named invitation file; the joined profile is ready to use."},
		"bare_checkout":      {"A bare repository has no working tree.", "Use a clone with a working tree, then rerun this command."},
		"promotion_ci":       {"The promoted commit's CI run did not pass.", "Read the printed run's logs before promoting or releasing again."},
		"tag_name":           {"The release tag name is invalid.", "Choose a valid Git tag name, then rerun oberth release."},
		"repository_missing": {"The bound repository is not registered.", "Run oberth repo add from this checkout."},
		"host_unsaved":       {"This profile saved no Oberth server SSH host keys.", serverTrustRemedy},
		"host_missing":       {"The Oberth server's SSH host-key file was not found.", serverTrustRemedy},
		"host_link":          {"The Oberth server's SSH host-key file is a symbolic link.", "Select the named regular trusted host-key file directly."},
		"host_kind":          {"The Oberth server's SSH host-key path is not a regular file.", "Select a regular trusted host-key file."},
		"host_mode":          {"The Oberth server's SSH host-key file allows other users to write.", "Remove group and other write permission from the named trusted file."},
		"host_read":          {"Cannot read the Oberth server's SSH host-key file.", "Check the named file's owner and read permission."},
		"agent":              {"Cannot resolve the running Oberth executable.", "Reinstall Oberth at an absolute executable path, then run oberth mcp."},
		"repo_executable":    {"Cannot bind this checkout with the running Oberth executable.", "Use the installed oberth command, or reinstall Oberth, then run oberth repo add."},
		"client_shim":        {"Cannot update the profile's Git helper.", "Remove the named helper, then run oberth repo add from this checkout."},
		"join_binding":       {"This checkout is not bound to the invitation's installation.", "Use a fresh clone, or run oberth repo add here with the intended profile before joining."},
		"git":                {"Checkout operation failed.", "Run this command in the bound repository and check its Git remote."},
		"binding":            {"This checkout has no repository binding.", "Run oberth repo add from this checkout."},
		"input":              {"The input file or value is invalid.", "Use a readable regular file inside the checkout."},
		"log_cursor":         {"Log cursor did not advance.", "Report the server response; log following has stopped."},
		"cursor":             {"Audit cursor did not advance.", "Report the server response; the audit tail has stopped."},
		"release":            {"Release run or publication failed.", "Read the release run logs and publication receipt before trying again."},
		"release_used":       {"Release failed; its version is used.", "An administrator can rerun it to keep the version, or release the next version."},
		"release_unrun":      {"The release tag already exists, but it has no release run.", "Its version is used; release the next version."},
		"release_head":       {"The checkout is not at the default-branch head.", "Check out the tested default-branch commit, then retry the release."},
		"release_tag":        {"The release tag points to another commit.", "Use a new tag; an existing release tag is never moved."},
		"release_checks":     {"Release checks refused the proposed tag.", "Apply the printed remedies, then rerun the same oberth release command."},
		"green":              {"The default branch does not have a passing run.", "Wait for the main run to pass before releasing."},
		"promotion":          {"Promotion failed.", "Read the promotion and run result, correct the problem and promote again."},
		"retesting":          {"The default branch moved; the promotion is being retested.", "See its state with the printed command; promoting again is not needed."},
		"publication":        {"The promoted commit is not recorded as published.", "Read its reason in oberth publication list; retry a failed one with oberth publication retry."},
		"starter_kind":       {"The pipeline path is not a regular file.", "Move the named path aside, then rerun oberth init."},
		"exists":             {"The output path already exists or is a link.", "Choose a new path, or use --force for an authored pipeline file."},
		"contract":           {"A required service protocol is unavailable.", "Use a server and CLI build with the required operation contract."},
	}
}

type flowRequest struct {
	options commonOptions
	args    []string
}

func (a application) executeFlow(ctx context.Context, command flowCommand, request flowRequest) int {
	s := &flowSession{app: a, ctx: ctx, options: request.options}
	defer s.close()
	err := command.run(s, request.args)
	if err != nil {
		return s.failed(err)
	}
	if request.options.json && len(s.last) > 0 {
		if err = a.render(s.last, false, request.options); err != nil {
			return a.failed(err)
		}
	}
	return 0
}
