package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/recordjson"
	"github.com/oberthci/oberth/internal/service"
)

// The agent loop on the command line: run_wait, events_since/events_wait
// and sync_status, for agents and scripts that use the CLI rather than an
// MCP client. Each blocks server-side; the client deadline follows the tool's
// own timeout (api.ToolTimeouts), so a 600 s wait is never cut at 60 s.

const runWaitUsage = "Usage: oberth run wait <run-id> [--until finished|held|any] [--timeout 600] [--json]\n" +
	"         [--url <url>] [--token-file <path>] [--ca-file <path>]\n" +
	"Block until the run finishes (or, with --until held, parks a retry for assisted continuation;\n" +
	"--until any: changes in any way), at most --timeout seconds (default and maximum 600).\n" +
	"Exit status 0 whatever the run's outcome; read the state and status lines, or --json."

const eventsUsage = "Usage: oberth events [--since <event-id>] [--wait] [--timeout 600] [--kind <kind>]... [--repo <repo>]...\n" +
	"         [--run <run-id>]... [--ref-glob <pattern>] [--own] [--limit N] [--json]\n" +
	"         [--url <url>] [--token-file <path>] [--ca-file <path>]\n" +
	"Print agent events after --since (default: from the oldest retained). With --wait, block until\n" +
	"at least one matching event arrives (default: one after the newest) or --timeout seconds pass.\n" +
	"The last line names the cursor to pass as --since next time."

const syncStatusUsage = "Usage: oberth sync status [--lock <key>] [--since 24h] [--limit N] [--json]\n" +
	"         [--url <url>] [--token-file <path>] [--ca-file <path>]\n" +
	"Native Argo semaphore and mutex holders and waiters now, and their stored history."

type agentLoopFlags struct {
	flags     *flag.FlagSet
	asJSON    *bool
	transport func() []string
}

func newAgentLoopFlags(name string) agentLoopFlags {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	return agentLoopFlags{flags: flags, asJSON: flags.Bool("json", false, "emit the tool's JSON result unchanged"), transport: mcpTransportFlags(flags)}
}

// parse returns the positional arguments.
func (options agentLoopFlags) parse(arguments []string, usage string) ([]string, error) {
	positional, help, err := parseOperatorFlags(options.flags, arguments)
	if err != nil {
		return nil, err
	}
	if help {
		return nil, commandFlagHelp(options.flags, usage)
	}
	return positional, nil
}

func (options agentLoopFlags) client(name string) (remoteRunCanceller, error) {
	config, err := parseMCPFlags(name, options.transport(), 0)
	if err != nil {
		return remoteRunCanceller{}, err
	}
	return remoteRunCanceller{config: config}, nil
}

func emitAgentLoopJSON(output io.Writer, raw json.RawMessage) error {
	_, err := fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
	return err
}

func runRunWait(ctx context.Context, arguments []string, output io.Writer) error {
	options := newAgentLoopFlags("run wait")
	until := options.flags.String("until", "finished", "finished, held or any")
	timeout := options.flags.Int("timeout", 0, "wait bound in seconds, 1 to 600 (default 600)")
	positional, err := options.parse(arguments, runWaitUsage)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("%w: run wait <run-id>", errUsage)
	}
	if err := refuseVerbAsRunID("run wait", positional[0], "<run-id>"); err != nil {
		return err
	}
	if *timeout < 0 || *timeout > 600 {
		return fmt.Errorf("%w: --timeout is 1 to 600 seconds", errUsage)
	}
	remote, err := options.client("run wait")
	if err != nil {
		return err
	}
	toolArguments := map[string]any{"id": positional[0], "until": *until}
	if *timeout > 0 {
		toolArguments["timeout_s"] = *timeout
	}
	raw, err := remote.call(ctx, "run_wait", toolArguments)
	if err != nil {
		return err
	}
	if *options.asJSON {
		return emitAgentLoopJSON(output, raw)
	}
	var response service.RunWaitResponse
	if err := recordjson.Unmarshal(raw, &response); err != nil {
		return emitAgentLoopJSON(output, raw)
	}
	return renderRunWait(output, response)
}

func renderRunWait(output io.Writer, response service.RunWaitResponse) error {
	run := response.Run
	var text strings.Builder
	fmt.Fprintf(&text, "state:   %s after %ds\n", response.State, response.WaitedS)
	fmt.Fprintf(&text, "run:     %s %s %s@%s\n", run.ID, run.Repo, run.Ref, shortSHA(run.SHA))
	status := run.DisplayStatus
	if status == "" {
		status = string(run.Status)
	}
	if run.DisplayReason != "" {
		status += " · " + run.DisplayReason
	}
	fmt.Fprintf(&text, "status:  %s\n", status)
	for _, hold := range response.Holds {
		line := fmt.Sprintf("held:    step %s (pod %s) since %s", hold.Step, hold.Pod, hold.HeldSince.UTC().Format(time.RFC3339))
		if hold.AbandonAt != nil {
			line += "; abandoned at " + hold.AbandonAt.UTC().Format(time.RFC3339) + " unless serviced (oberth run hold ...)"
		}
		fmt.Fprintln(&text, line)
	}
	if hint := response.RetryHint; hint != nil {
		fmt.Fprintf(&text, "retry:   %s (%s): %s\n", hint.Class, hint.Cause, hint.Action)
	}
	_, err := io.WriteString(output, text.String())
	return err
}

func runEvents(ctx context.Context, arguments []string, output io.Writer) error {
	options := newAgentLoopFlags("events")
	since := options.flags.Int64("since", -1, "print events after this event ID (the cursor a previous call printed)")
	wait := options.flags.Bool("wait", false, "block until at least one matching event arrives (events_wait)")
	timeout := options.flags.Int("timeout", 0, "with --wait: bound in seconds, 1 to 600 (default 600)")
	limit := options.flags.Int("limit", 0, "page size before filtering, 1 to 200 (default 100)")
	own := options.flags.Bool("own", false, "only events about this uplink's runs and promotions")
	refsGlob := options.flags.String("ref-glob", "", "shell-style pattern on the ref, e.g. land/*")
	var kinds, repos, runs repeatedFlag
	options.flags.Var(&kinds, "kind", "event kind (repeatable), e.g. run.finished, run.held")
	options.flags.Var(&repos, "repo", "repository (repeatable); use upstream/org/name when ambiguous")
	options.flags.Var(&runs, "run", "with --wait: only events about this run (repeatable)")
	positional, err := options.parse(arguments, eventsUsage)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: events takes no positional arguments", errUsage)
	}
	if *since < -1 {
		return fmt.Errorf("%w: --since is an event ID, 0 or more", errUsage)
	}
	if !*wait && (*timeout != 0 || len(runs) > 0) {
		return fmt.Errorf("%w: --timeout and --run apply to --wait", errUsage)
	}
	if *timeout < 0 || *timeout > 600 || *limit < 0 || *limit > 200 {
		return fmt.Errorf("%w: --timeout is 1 to 600 seconds and --limit 1 to 200", errUsage)
	}
	remote, err := options.client("events")
	if err != nil {
		return err
	}
	toolArguments := map[string]any{}
	if *since >= 0 {
		toolArguments["after_id"] = *since
	} else if !*wait {
		toolArguments["after_id"] = 0
	}
	if *limit > 0 {
		toolArguments["limit"] = *limit
	}
	if *own {
		toolArguments["own"] = true
	}
	if *refsGlob != "" {
		toolArguments["refs_glob"] = *refsGlob
	}
	if len(kinds) > 0 {
		toolArguments["kinds"] = []string(kinds)
	}
	if len(repos) > 0 {
		toolArguments["repos"] = []string(repos)
	}
	tool := "events_since"
	if *wait {
		tool = "events_wait"
		if len(runs) > 0 {
			toolArguments["run_ids"] = []string(runs)
		}
		if *timeout > 0 {
			toolArguments["timeout_s"] = *timeout
		}
	}
	raw, err := remote.call(ctx, tool, toolArguments)
	if err != nil {
		return err
	}
	if *options.asJSON {
		return emitAgentLoopJSON(output, raw)
	}
	var page service.EventsWaitResponse
	if err := recordjson.Unmarshal(raw, &page); err != nil {
		return emitAgentLoopJSON(output, raw)
	}
	return renderEvents(output, page)
}

func renderEvents(output io.Writer, page service.EventsWaitResponse) error {
	var text strings.Builder
	if page.Gap {
		fmt.Fprintln(&text, "gap: events after the cursor were pruned (agentEvents.retention)")
	}
	for _, event := range page.Events {
		fmt.Fprintf(&text, "%-6d %s %-18s %s\n", event.ID, event.At.UTC().Format(time.RFC3339), event.Kind, event.Summary)
	}
	switch {
	case page.TimedOut:
		fmt.Fprintf(&text, "no matching event within %ds\n", page.WaitedS)
	case len(page.Events) == 0:
		fmt.Fprintln(&text, "no events")
	}
	more := ""
	if page.HasMore {
		more = " (more after it)"
	}
	fmt.Fprintf(&text, "next: oberth events --since %d%s\n", page.NextAfterID, more)
	_, err := io.WriteString(output, text.String())
	return err
}

func runSync(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 || wantsHelp(arguments[:1]) {
		_, err := fmt.Fprintln(output, syncStatusUsage)
		return err
	}
	if arguments[0] != "status" {
		return fmt.Errorf("%w: sync status (unknown subcommand %q)", errUsage, arguments[0])
	}
	options := newAgentLoopFlags("sync status")
	lock := options.flags.String("lock", "", "one lock key, e.g. Mutex/oberth-release")
	since := options.flags.String("since", "", "history window such as 6h or 7d (default 24h)")
	limit := options.flags.Int("limit", 0, "maximum stored intervals, newest first (default 2000)")
	positional, err := options.parse(arguments[1:], syncStatusUsage)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return fmt.Errorf("%w: sync status takes no positional arguments", errUsage)
	}
	remote, err := options.client("sync status")
	if err != nil {
		return err
	}
	toolArguments := map[string]any{}
	if *lock != "" {
		toolArguments["lock"] = *lock
	}
	if *since != "" {
		toolArguments["since"] = *since
	}
	if *limit > 0 {
		toolArguments["limit"] = *limit
	}
	raw, err := remote.call(ctx, "sync_status", toolArguments)
	if err != nil {
		return err
	}
	if *options.asJSON {
		return emitAgentLoopJSON(output, raw)
	}
	var status service.SyncStatusResponse
	if err := recordjson.Unmarshal(raw, &status); err != nil {
		return emitAgentLoopJSON(output, raw)
	}
	return renderSyncStatus(output, status)
}

func renderSyncStatus(output io.Writer, status service.SyncStatusResponse) error {
	var text strings.Builder
	if !status.Observing {
		fmt.Fprintln(&text, "not observing: the server samples no Workflow synchronization yet")
	}
	members := func(values []service.SyncMember) string {
		if len(values) == 0 {
			return "-"
		}
		names := make([]string, len(values))
		for index, member := range values {
			names[index] = fmt.Sprintf("%s (%ds)", shortRunID(member.RunID), member.Seconds)
		}
		return strings.Join(names, ", ")
	}
	if len(status.Locks) == 0 {
		fmt.Fprintln(&text, "no lock is held or waited for")
	}
	for _, lock := range status.Locks {
		fmt.Fprintf(&text, "%s\n  holders: %s\n  waiters: %s\n", lock.Lock, members(lock.Holders), members(lock.Waiters))
	}
	for _, history := range status.History {
		fmt.Fprintf(&text, "history %s: %d holder intervals, at most %d at once; %d waits, longest %ds, median %ds\n",
			history.Lock, history.HolderIntervals, history.MaxConcurrentHolders, history.Waits.Count, history.Waits.MaxSeconds, history.Waits.P50Seconds)
	}
	if status.Unobserved > 0 {
		fmt.Fprintf(&text, "%d active run(s) not yet observed\n", status.Unobserved)
	}
	_, err := io.WriteString(output, text.String())
	return err
}

func shortRunID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
