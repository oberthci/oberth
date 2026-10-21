package main

// `oberth audit [--tail]` (#1104): the remote form of the admin-only MCP
// audit_list tool. `audit head` and `audit record` stay in-pod.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/service"
)

const auditUsage = "Usage: oberth audit [--tail] [--action <prefix>] [--actor <identity>] [--repo <repo>] [--since <time|24h>]\n" +
	"         [--until <time|24h>] [--limit N] [--before ID] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\n" +
	"       oberth audit head [--database <path>]   (in-pod)\n" +
	"       oberth audit record <action> [--details JSON]   (in-pod)"

// auditTailInterval is how often --tail asks for new entries.
var auditTailInterval = 2 * time.Second

// mcpTransportFlags registers the common MCP transport flags on a flag set
// and returns a function building the arguments parseMCPFlags understands.
func mcpTransportFlags(flags *flag.FlagSet) func() []string {
	url := flags.String("url", "", "MCP URL")
	tokenFile := flags.String("token-file", "", "token file")
	caFile := flags.String("ca-file", "", "CA file")
	return func() []string {
		var transport []string
		for name, value := range map[string]string{"--url": *url, "--token-file": *tokenFile, "--ca-file": *caFile} {
			if value != "" {
				transport = append(transport, name+"="+value)
			}
		}
		sort.Strings(transport)
		return transport
	}
}

func runAuditList(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, auditUsage)
		return err
	}
	flags := flag.NewFlagSet("audit", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	tail := flags.Bool("tail", false, "show the newest entries oldest first, then follow new ones")
	asJSON := flags.Bool("json", false, "emit the server's payload unchanged")
	action := flags.String("action", "", "action prefix")
	actor := flags.String("actor", "", "exact actor identity")
	repo := flags.String("repo", "", "repository")
	since := flags.String("since", "", "RFC 3339 time or duration such as 24h")
	until := flags.String("until", "", "RFC 3339 time or duration such as 24h")
	limit := flags.Int("limit", 0, "entries per page (default 50, tail default 20)")
	before := flags.Int64("before", 0, "exclusive audit ID cursor")
	transport := mcpTransportFlags(flags)
	if err := flags.Parse(permuteFlagsFirst(arguments)); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: audit takes flags only", errUsage)
	}
	config, err := parseMCPFlags("audit", transport(), 0)
	if err != nil {
		return err
	}
	remote := remoteRunCanceller{config: config}
	toolArguments := map[string]any{}
	for name, value := range map[string]string{"action": *action, "actor": *actor, "repo": *repo, "since": *since, "until": *until} {
		if strings.TrimSpace(value) != "" {
			toolArguments[name] = value
		}
	}
	if *limit > 0 {
		toolArguments["limit"] = *limit
	}
	if *before > 0 {
		toolArguments["before"] = *before
	}
	if *tail {
		if *before > 0 {
			return fmt.Errorf("%w: --tail and --before are exclusive", errUsage)
		}
		if *limit == 0 {
			toolArguments["limit"] = 20
		}
		return tailAudit(ctx, remote, toolArguments, *asJSON, output)
	}
	raw, err := remote.call(ctx, "audit_list", toolArguments)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := fmt.Fprintln(output, strings.TrimSpace(string(raw)))
		return err
	}
	var response service.AuditListResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode audit_list: %w", err)
	}
	if err := writeAuditEntries(output, response.Entries, true); err != nil {
		return err
	}
	if response.NextBefore > 0 {
		_, err = fmt.Fprintf(output, "more: --before %d\n", response.NextBefore)
	}
	return err
}

type auditCaller interface {
	call(context.Context, string, any) (json.RawMessage, error)
}

func fetchAudit(ctx context.Context, remote auditCaller, arguments map[string]any) (service.AuditListResponse, error) {
	raw, err := remote.call(ctx, "audit_list", arguments)
	if err != nil {
		return service.AuditListResponse{}, err
	}
	var response service.AuditListResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return service.AuditListResponse{}, fmt.Errorf("decode audit_list: %w", err)
	}
	return response, nil
}

// tailAudit prints the newest entries oldest first, then polls for entries
// past the highest ID seen until the context ends. Filters apply throughout.
func tailAudit(ctx context.Context, remote auditCaller, arguments map[string]any, asJSON bool, output io.Writer) error {
	response, err := fetchAudit(ctx, remote, arguments)
	if err != nil {
		return err
	}
	last := auditHighWater(response)
	emit := func(entries []service.AuditEntryView) error {
		if asJSON {
			for i := len(entries) - 1; i >= 0; i-- {
				encoded, err := json.Marshal(entries[i])
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(output, "%s\n", encoded); err != nil {
					return err
				}
			}
			return nil
		}
		return writeAuditEntries(output, entries, false)
	}
	if err := emit(response.Entries); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(auditTailInterval):
		}
		fresh, highWater, err := pollAudit(ctx, remote, arguments, last)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := emit(fresh); err != nil {
			return err
		}
		last = highWater
	}
}

// The head is read before the entries, so the page may include newer rows.
func auditHighWater(page service.AuditListResponse) int64 {
	high := page.HeadID
	for _, entry := range page.Entries {
		high = max(high, entry.ID)
	}
	return high
}

func pollAudit(ctx context.Context, remote auditCaller, arguments map[string]any, last int64) ([]service.AuditEntryView, int64, error) {
	poll := map[string]any{}
	for name, value := range arguments {
		poll[name] = value
	}
	delete(poll, "before")
	poll["limit"] = 200
	page, err := fetchAudit(ctx, remote, poll)
	if err != nil {
		return nil, last, err
	}
	// Later page heads can include appends outside this scan. Leave those
	// for the next poll instead of advancing past entries we have not read.
	highWater := max(last, auditHighWater(page))
	var fresh []service.AuditEntryView
	var before int64
	for {
		for _, entry := range page.Entries {
			if entry.ID > last {
				fresh = append(fresh, entry)
			}
		}
		if page.NextBefore <= last {
			break
		}
		if before != 0 && page.NextBefore >= before {
			return nil, last, fmt.Errorf("audit_list cursor did not advance")
		}
		before = page.NextBefore
		poll["before"] = before
		page, err = fetchAudit(ctx, remote, poll)
		if err != nil {
			return nil, last, err
		}
	}
	return fresh, highWater, nil
}

func writeAuditEntries(output io.Writer, entries []service.AuditEntryView, newestFirst bool) error {
	write := func(entry service.AuditEntryView) error {
		details := strings.TrimSpace(string(entry.Details))
		if details == "{}" || details == "null" {
			details = ""
		}
		_, err := fmt.Fprintf(output, "%-6d %s  %-28s %-24s %s/%s  %s\n", entry.ID, entry.Time, entry.Action, entry.Actor,
			entry.ResourceType, entry.ResourceID, details)
		return err
	}
	if newestFirst {
		for _, entry := range entries {
			if err := write(entry); err != nil {
				return err
			}
		}
		return nil
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if err := write(entries[i]); err != nil {
			return err
		}
	}
	return nil
}
