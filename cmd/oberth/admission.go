package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

const admissionUsage = "Usage: oberth admission [--json] | oberth admission release <run-id> [--force] [--json]\n" +
	"       oberth admission groups [--json]\n" +
	"       oberth admission hold on|off --reason <reason> [--allow tag,release,promotion] [--duration 2h] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]"

// runAdmission prints the server's admission gate, or releases one stale
// reservation (#1099). The gate lives in the server process, so both go
// through MCP: system_status for the view, admission_release to release.
func runAdmission(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) > 0 && arguments[0] == "hold" {
		return runAdmissionHold(ctx, arguments[1:], output)
	}
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, admissionUsage)
		return err
	}
	flags := flag.NewFlagSet("admission", flag.ContinueOnError)
	forceFlag := flags.Bool("force", false, "release a stale reservation")
	jsonFlag := flags.Bool("json", false, "emit JSON")
	transportFlags := mcpTransportFlags(flags)
	positional, _, err := parseOperatorFlags(flags, arguments)
	if err != nil {
		return err
	}
	force, asJSON := *forceFlag, *jsonFlag
	transport := transportFlags()

	groups := len(positional) == 1 && positional[0] == "groups"
	release := len(positional) > 0 && positional[0] == "release"
	switch {
	case release && len(positional) != 2:
		return fmt.Errorf("%w: admission release <run-id>", errUsage)
	case !release && (!groups && len(positional) != 0 || force):
		return fmt.Errorf("%w: %s", errUsage, admissionUsage)
	}
	config, err := parseMCPFlags("admission", transport, 0)
	if err != nil {
		return err
	}
	remote := remoteRunCanceller{config: config}
	if groups {
		raw, err := remote.call(ctx, "concurrency_groups", map[string]any{})
		if err != nil {
			return err
		}
		response, err := decodeAdmissionGroups(raw)
		if err != nil {
			return err
		}
		if asJSON {
			_, err = fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
			return err
		}
		table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(table, "GROUP\tLIMIT\tHOLDERS\tWAITERS")
		for _, g := range response.Groups {
			_, _ = fmt.Fprintf(table, "%s\t%d\t%s\t%s\n", g.Name, g.Limit, dash(strings.Join(g.Holders, ", ")), dash(strings.Join(g.Waiting, ", ")))
		}
		return table.Flush()
	}
	if release {
		arguments := map[string]any{"run_id": positional[1]}
		if force {
			arguments["force"] = true
		}
		raw, err := remote.call(ctx, "admission_release", arguments)
		if err != nil {
			return err
		}
		var released model.AdmissionRelease
		if asJSON || json.Unmarshal(raw, &released) != nil || released.RunID == "" {
			_, err = fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
			return err
		}
		line := fmt.Sprintf("%s  released (%s, weight %d", released.RunID, released.State, released.Weight)
		if released.Group != "" {
			line += ", group " + released.Group
		}
		line += ")"
		if released.Warning != "" {
			line += "\n  warning: " + released.Warning
		}
		_, err = fmt.Fprintln(output, line)
		return err
	}
	raw, err := remote.call(ctx, "system_status", map[string]any{})
	if err != nil {
		return err
	}
	var status struct {
		Admission *model.AdmissionState `json:"admission"`
	}
	if err := json.Unmarshal(raw, &status); err != nil || status.Admission == nil {
		return fmt.Errorf("the server does not report admission state (it predates #1099)")
	}
	if asJSON {
		encoded, err := json.MarshalIndent(status.Admission, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "%s\n", encoded)
		return err
	}
	return renderAdmission(output, status.Admission, time.Now())
}

func decodeAdmissionGroups(raw []byte) (api.ConcurrencyGroupsResponse, error) {
	var response api.ConcurrencyGroupsResponse
	groups := bytes.TrimSpace(raw)
	// Retain the CLI's existing text-array compatibility for older responses,
	// without changing MCP's object-only result validation.
	if len(groups) == 0 || groups[0] != '[' {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return response, fmt.Errorf("invalid concurrency_groups response: %w", err)
		}
		groups = bytes.TrimSpace(envelope["groups"])
	}
	if len(groups) == 0 || groups[0] != '[' {
		return response, fmt.Errorf("invalid concurrency_groups response: groups must be an array")
	}
	if err := json.Unmarshal(groups, &response.Groups); err != nil {
		return response, fmt.Errorf("invalid concurrency_groups response: %w", err)
	}
	return response, nil
}

func renderAdmission(output io.Writer, state *model.AdmissionState, now time.Time) error {
	if state.Capacity > 0 {
		if _, err := fmt.Fprintf(output, "weight %d/%d used, %d run(s) admitted (ceiling %d)\n", state.Used, state.Capacity, state.Runs, state.MaxRuns); err != nil {
			return err
		}
	}
	if p := state.Pressure; p != nil {
		if p.Fallback != "" {
			_, _ = fmt.Fprintln(output, p.Fallback)
		} else if p.Sample != nil {
			_, _ = fmt.Fprintf(output, "pressure some avg10: cpu %.1f%%, io %.1f%%; holding=%t gate=%s\n", p.Sample.CPU.SomeAvg10, p.Sample.IO.SomeAvg10, p.Holding, dash(p.Resource))
		}
	}
	if state.Blocker != nil {
		line := fmt.Sprintf("queue head: %s/%s (%s)", state.Blocker.Repo, state.Blocker.Ref, state.Blocker.Reason())
		if backfill := state.Blocker.Backfill(); backfill != "" {
			line += "; " + backfill
		}
		if _, err := fmt.Fprintln(output, line); err != nil {
			return err
		}
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "RUN\tSTATE\tSIZE\tWEIGHT\tREPO/REF\tGROUP\tFOR")
	for _, reservation := range state.Reservations {
		age := "-"
		if !reservation.Since.IsZero() {
			age = now.Sub(reservation.Since).Truncate(time.Second).String()
		}
		label := reservation.State
		if reservation.Backfilled {
			label += " (backfill)"
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s/%s\t%s\t%s\n", reservation.RunID, label,
			dash(reservation.Size), reservation.Weight, reservation.Repo, reservation.Ref, dash(reservation.Group), age)
	}
	return writer.Flush()
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
