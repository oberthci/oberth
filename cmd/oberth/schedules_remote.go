package main

// `oberth schedules list|pause|resume|trigger` (#1106): the remote form of the
// schedule_* MCP tools. `oberth schedules [repository]` without a subcommand
// stays the in-pod listing.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/service"
)

const schedulesRemoteUsage = "Usage: oberth schedules list [repo] [--json]\n" +
	"       oberth schedules pause <repo> <entry> [--reason <text>]\n" +
	"       oberth schedules resume <repo> <entry>\n" +
	"       oberth schedules trigger <repo> <entry> [--force]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]\n" +
	"       oberth schedules [repository]   (in-pod listing of the schedule file)"

func runRemoteSchedules(ctx context.Context, subcommand string, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, schedulesRemoteUsage)
		return err
	}
	flags := flag.NewFlagSet("schedules "+subcommand, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "emit the server's payload unchanged")
	reason := flags.String("reason", "", "why the entry is paused")
	force := flags.Bool("force", false, "run despite active runs or a pause")
	transport := scheduleTransportFlags(flags)
	if err := flags.Parse(permuteFlagsFirst(arguments)); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	positional := flags.Args()
	toolArguments := map[string]any{}
	tool := "schedule_" + subcommand
	switch subcommand {
	case "list":
		if len(positional) > 1 {
			return fmt.Errorf("%w: schedules list [repo]", errUsage)
		}
		if len(positional) == 1 {
			toolArguments["repo"] = positional[0]
		}
	default:
		if len(positional) != 2 {
			return fmt.Errorf("%w: schedules %s <repo> <entry>", errUsage, subcommand)
		}
		toolArguments["repo"], toolArguments["entry"] = positional[0], positional[1]
		if subcommand == "pause" && strings.TrimSpace(*reason) != "" {
			toolArguments["reason"] = *reason
		}
		if subcommand == "trigger" && *force {
			toolArguments["force"] = true
		}
	}
	config, err := parseMCPFlags("schedules "+subcommand, transport(), 0)
	if err != nil {
		return err
	}
	raw, err := remoteRunCanceller{config: config}.call(ctx, tool, toolArguments)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := fmt.Fprintln(output, strings.TrimSpace(string(raw)))
		return err
	}
	switch subcommand {
	case "list":
		var response service.ScheduleListResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			return fmt.Errorf("decode schedule_list: %w", err)
		}
		return writeScheduleList(output, response)
	case "trigger":
		var response service.ScheduleTriggerResponse
		if err := json.Unmarshal(raw, &response); err != nil {
			return fmt.Errorf("decode schedule_trigger: %w", err)
		}
		_, err := fmt.Fprintf(output, "%s  %s/%s queued at %s on %s\n", response.RunID, response.Repo, response.Entry, shortSHA(response.SHA), response.Branch)
		return err
	default:
		var response struct {
			Repo    string `json:"repo"`
			Entry   string `json:"entry"`
			Paused  bool   `json:"paused"`
			Changed bool   `json:"changed"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return fmt.Errorf("decode %s: %w", tool, err)
		}
		state := "running"
		if response.Paused {
			state = "paused"
		}
		note := ""
		if !response.Changed {
			note = " (unchanged)"
		}
		_, err := fmt.Fprintf(output, "%s/%s %s%s\n", response.Repo, response.Entry, state, note)
		return err
	}
}

func writeScheduleList(output io.Writer, response service.ScheduleListResponse) error {
	if len(response.Schedules) == 0 {
		_, err := fmt.Fprintln(output, "no schedules")
		return err
	}
	for _, repo := range response.Schedules {
		if _, err := fmt.Fprintf(output, "%s (%s)\n", repo.Repo, repo.DefaultBranch); err != nil {
			return err
		}
		if repo.Error != "" {
			if _, err := fmt.Fprintf(output, "  refused: %s\n", repo.Error); err != nil {
				return err
			}
		}
		for _, entry := range repo.Entries {
			last := "never run"
			if entry.LastOutcome != "" {
				last = entry.LastOutcome + " " + entry.LastFiredAt
			}
			line := fmt.Sprintf("  %-20s %-16s next %s  last %s", entry.Name, entry.Cron, entry.NextAt, last)
			if entry.Paused != nil {
				line += fmt.Sprintf("  PAUSED by %s at %s", entry.Paused.By, entry.Paused.At)
				if entry.Paused.Reason != "" {
					line += ": " + entry.Paused.Reason
				}
			}
			if _, err := fmt.Fprintln(output, line); err != nil {
				return err
			}
		}
		for _, refusal := range repo.Refused {
			if _, err := fmt.Fprintf(output, "  %-20s REFUSED: %s\n", refusal.Name, refusal.Reason); err != nil {
				return err
			}
		}
	}
	return nil
}

// scheduleTransportFlags registers the common MCP transport flags and returns
// a function building the arguments parseMCPFlags understands.
func scheduleTransportFlags(flags *flag.FlagSet) func() []string {
	url := flags.String("url", "", "MCP URL")
	tokenFile := flags.String("token-file", "", "token file")
	caFile := flags.String("ca-file", "", "CA file")
	return func() []string {
		var transport []string
		for _, option := range []struct{ name, value string }{{"--url", *url}, {"--token-file", *tokenFile}, {"--ca-file", *caFile}} {
			if option.value != "" {
				transport = append(transport, option.name+"="+option.value)
			}
		}
		return transport
	}
}
