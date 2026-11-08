package main

// `oberth branch prune|deletions|resolve` and `oberth outbox` (#1490, #1500):
// remote forms of the unscoped-admin MCP tools branch_delete_many,
// branch_deletion_list, branch_deletion_resolve and receive_outbox.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
)

const branchUsage = "Usage: oberth branch prune <repository> [--merged-into <default-branch>] [--older-than <30d|72h>] [--exclude-prefix <prefix>]...\n" +
	"         [--branch <name>=<sha>]... [--allow-unmerged] [--report] [--confirm --plan-id <id>] [--confirm-reserved <upstream/org/repo:branch>]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\n" +
	"       oberth branch deletions [--repo <repository>] [--status pending|propagated|failed|skipped] [--batch <id>]\n" +
	"         [--limit N] [--before N] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\n" +
	"       oberth branch resolve <repository> --reason <text> [--id <record-id>]...\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\n" +
	"prune is a dry run unless --confirm names the dry run's plan ID. Mirror refs are deleted at once;\n" +
	"upstream deletion follows asynchronously. Read per-branch upstream state with branch deletions.\n" +
	"Each planned branch is classed merged, patch-equivalent, unique or unchecked; --report also lists\n" +
	"the kept ones. resolve closes failed deletion records (no ref changes); without --id only those\n" +
	"whose branch is gone from the mirror."

const outboxUsage = "Usage: oberth outbox list|show|retry|abandon [<upstream/org/repo>] [--id <record-id>] [--reason <text>]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\n" +
	"Repair a receive record that blocks its repository: show it, retry delivery, or abandon it\n" +
	"(moved aside under abandoned/, audited). Unscoped admin only."

type repeatedFlag []string

func (values *repeatedFlag) String() string { return strings.Join(*values, ",") }
func (values *repeatedFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func runBranch(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 || wantsHelp(arguments[:1]) {
		_, err := fmt.Fprintln(output, branchUsage)
		return err
	}
	switch arguments[0] {
	case "prune":
		return runBranchPrune(ctx, arguments[1:], output)
	case "deletions":
		return runBranchDeletions(ctx, arguments[1:], output)
	case "resolve":
		return runBranchResolve(ctx, arguments[1:], output)
	default:
		return fmt.Errorf("%w: %s", errUsage, branchUsage)
	}
}

func runBranchPrune(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, branchUsage)
		return err
	}
	flags := flag.NewFlagSet("branch prune", flag.ContinueOnError)
	transportFlags := mcpTransportFlags(flags)
	mergedInto := flags.String("merged-into", "", "select branches merged or patch-equivalent into this default branch")
	olderThan := flags.String("older-than", "", "select branches whose last commit is older than this age")
	allowUnmerged := flags.Bool("allow-unmerged", false, "also delete unmerged branches")
	confirm := flags.Bool("confirm", false, "execute the plan named by --plan-id")
	planID := flags.String("plan-id", "", "plan ID from the dry run")
	confirmReserved := flags.String("confirm-reserved", "", "typed confirmation to delete one reserved branch")
	report := flags.Bool("report", false, "dry run only: also list kept (unique or unchecked) branches")
	var excludes, branches repeatedFlag
	flags.Var(&excludes, "exclude-prefix", "skip branches with this prefix (repeatable)")
	flags.Var(&branches, "branch", "explicit <name>=<expected-sha> (repeatable)")
	positional, _, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	if len(positional) != 1 || positional[0] == "" {
		return fmt.Errorf("%w: %s", errUsage, branchUsage)
	}
	request := map[string]any{"repo": positional[0]}
	if len(branches) > 0 {
		list := make([]map[string]string, 0, len(branches))
		for _, value := range branches {
			name, sha, ok := strings.Cut(value, "=")
			if !ok || name == "" || sha == "" {
				return fmt.Errorf("%w: --branch takes <name>=<expected-sha>", errUsage)
			}
			list = append(list, map[string]string{"branch": name, "expected_sha": sha})
		}
		request["branches"] = list
	}
	for key, value := range map[string]string{"merged_into": *mergedInto, "older_than": *olderThan, "plan_id": *planID, "confirm_reserved": *confirmReserved} {
		if value != "" {
			request[key] = value
		}
	}
	if len(excludes) > 0 {
		request["exclude_prefixes"] = []string(excludes)
	}
	if *allowUnmerged {
		request["allow_unmerged"] = true
	}
	if *confirm {
		request["confirm"] = true
	}
	if *report {
		request["report"] = true
	}
	config, err := parseMCPFlags("branch prune", transportFlags(), 0)
	if err != nil {
		return err
	}
	caller := remoteRunCanceller{config: config}
	raw, err := caller.call(ctx, "branch_delete_many", request)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, strings.TrimSpace(string(raw)))
	return err
}

func runBranchDeletions(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, branchUsage)
		return err
	}
	flags := flag.NewFlagSet("branch deletions", flag.ContinueOnError)
	transportFlags := mcpTransportFlags(flags)
	repo := flags.String("repo", "", "repository")
	status := flags.String("status", "", "pending, propagated, failed or skipped")
	batch := flags.String("batch", "", "prune batch ID or receive event ID")
	limit := flags.Int("limit", 0, "page size (default 50, max 200)")
	before := flags.Int64("before", 0, "next_before cursor from the previous page")
	positional, _, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: %s", errUsage, branchUsage)
	}
	request := map[string]any{}
	for key, value := range map[string]string{"repo": *repo, "status": *status, "batch": *batch} {
		if value != "" {
			request[key] = value
		}
	}
	if *limit > 0 {
		request["limit"] = *limit
	}
	if *before > 0 {
		request["before"] = *before
	}
	config, err := parseMCPFlags("branch deletions", transportFlags(), 0)
	if err != nil {
		return err
	}
	raw, err := (remoteRunCanceller{config: config}).call(ctx, "branch_deletion_list", request)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, strings.TrimSpace(string(raw)))
	return err
}

func runBranchResolve(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, branchUsage)
		return err
	}
	flags := flag.NewFlagSet("branch resolve", flag.ContinueOnError)
	transportFlags := mcpTransportFlags(flags)
	reason := flags.String("reason", "", "why the failed records are resolved")
	var ids repeatedFlag
	flags.Var(&ids, "id", "failed record ID from branch deletions (repeatable)")
	positional, _, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	if len(positional) != 1 || positional[0] == "" || strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("%w: %s", errUsage, branchUsage)
	}
	request := map[string]any{"repo": positional[0], "reason": *reason}
	if len(ids) > 0 {
		request["ids"] = []string(ids)
	}
	config, err := parseMCPFlags("branch resolve", transportFlags(), 0)
	if err != nil {
		return err
	}
	raw, err := (remoteRunCanceller{config: config}).call(ctx, "branch_deletion_resolve", request)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, strings.TrimSpace(string(raw)))
	return err
}

func runOutbox(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 || wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, outboxUsage)
		return err
	}
	flags := flag.NewFlagSet("outbox", flag.ContinueOnError)
	transportFlags := mcpTransportFlags(flags)
	id := flags.String("id", "", "record ID from show")
	reason := flags.String("reason", "", "why the record is abandoned")
	positional, _, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	if len(positional) == 0 || len(positional) > 2 {
		return fmt.Errorf("%w: %s", errUsage, outboxUsage)
	}
	request := map[string]any{"action": positional[0]}
	switch positional[0] {
	case "list":
		if len(positional) != 1 {
			return fmt.Errorf("%w: %s", errUsage, outboxUsage)
		}
	case "show", "retry", "abandon":
		if len(positional) != 2 {
			return fmt.Errorf("%w: %s", errUsage, outboxUsage)
		}
		request["repo"] = positional[1]
	default:
		return fmt.Errorf("%w: %s", errUsage, outboxUsage)
	}
	if *id != "" {
		request["id"] = *id
	}
	if *reason != "" {
		request["reason"] = *reason
	}
	config, err := parseMCPFlags("outbox", transportFlags(), 0)
	if err != nil {
		return err
	}
	raw, err := (remoteRunCanceller{config: config}).call(ctx, "receive_outbox", request)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, strings.TrimSpace(string(raw)))
	return err
}
