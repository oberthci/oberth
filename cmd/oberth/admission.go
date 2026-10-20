package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const admissionUsage = "Usage: oberth admission [--json] | oberth admission release <run-id> [--force] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]"

// runAdmission prints the server's admission gate, or releases one stale
// reservation (#1099). The gate lives in the server process, so both go
// through MCP: system_status for the view, admission_release to release.
func runAdmission(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, admissionUsage)
		return err
	}
	var transport, positional []string
	force, asJSON := false, false
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		name, inline, hasInline := strings.Cut(argument, "=")
		switch {
		case argument == "--force":
			force = true
		case argument == "--json":
			asJSON = true
		case name == "--url" || name == "--token-file" || name == "--ca-file":
			value := inline
			if !hasInline {
				if index+1 >= len(arguments) {
					return fmt.Errorf("%w: %s requires a value", errUsage, name)
				}
				index++
				value = arguments[index]
			}
			transport = append(transport, name, value)
		case strings.HasPrefix(argument, "-"):
			return fmt.Errorf("%w: unknown flag %q", errUsage, argument)
		default:
			positional = append(positional, argument)
		}
	}
	release := len(positional) > 0 && positional[0] == "release"
	switch {
	case release && len(positional) != 2:
		return fmt.Errorf("%w: admission release <run-id>", errUsage)
	case !release && (len(positional) != 0 || force):
		return fmt.Errorf("%w: %s", errUsage, admissionUsage)
	}
	config, err := parseMCPFlags("admission", transport, 0)
	if err != nil {
		return err
	}
	remote := admissionCaller{config: config}
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

func renderAdmission(output io.Writer, state *model.AdmissionState, now time.Time) error {
	if state.Capacity > 0 {
		if _, err := fmt.Fprintf(output, "weight %d/%d used, %d run(s) admitted (ceiling %d)\n", state.Used, state.Capacity, state.Runs, state.MaxRuns); err != nil {
			return err
		}
	}
	if state.Blocker != nil {
		if _, err := fmt.Fprintf(output, "queue head: %s/%s (%s)\n", state.Blocker.Repo, state.Blocker.Ref, state.Blocker.Reason()); err != nil {
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
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s/%s\t%s\t%s\n", reservation.RunID, reservation.State,
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

// admissionCaller calls one MCP tool for the admission commands and returns
// its JSON text.
type admissionCaller struct{ config mcpFlagResult }

func (caller admissionCaller) call(ctx context.Context, tool string, arguments map[string]any) (json.RawMessage, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	response, err := mcpRequest(ctx, caller.config, mcpMethodToolsCall, toolsCallParams{Name: tool, Arguments: encoded})
	if err != nil {
		return nil, err
	}
	if response.Error != nil {
		return nil, fmt.Errorf("server error: %s (code %d)", response.Error.Message, response.Error.Code)
	}
	var result toolsCallResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return nil, fmt.Errorf("invalid tools/call result: %w", err)
	}
	var text strings.Builder
	for _, content := range result.Content {
		if content.Type == "text" {
			text.WriteString(content.Text)
		}
	}
	if result.IsError {
		return nil, fmt.Errorf("%w: %s", errMCPToolError, strings.TrimSpace(text.String()))
	}
	return json.RawMessage(text.String()), nil
}
