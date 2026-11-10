package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/recordjson"
)

const admissionUsage = "Usage: oberth admission [--json] | oberth admission release <run-id> [--force] [--json]\n" +
	"       oberth admission hold on|off --reason <reason> [--allow tag,release,promotion] [--duration 2h] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]"

// removedAdmissionCommands name the subcommands v0.20.0 removed with the
// limits they served; each now lives in Argo (docs/where-limits-live.md).
var removedAdmissionCommands = map[string]string{
	"groups":   "oberth admission groups is gone since v0.20.0, with concurrency groups: declare a native Argo mutex or semaphore and read its holders and waiters with the sync_status MCP tool (docs/where-limits-live.md)",
	"capacity": "oberth admission capacity is gone since v0.20.0, with the Oberth run ceiling: set the Argo controller parallelism instead (oberth install --argo-parallelism, docs/where-limits-live.md)",
}

// runAdmission prints the server's admission gate, or releases one stale
// reservation (#1099). The gate lives in the server process, so both go
// through MCP: system_status for the view, admission_release to release.
func runAdmission(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) > 0 && arguments[0] == "hold" {
		return runAdmissionHold(ctx, arguments[1:], output)
	}
	if len(arguments) > 0 {
		if removed, ok := removedAdmissionCommands[arguments[0]]; ok {
			return fmt.Errorf("%w: %s", errUsage, removed)
		}
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
	remote := remoteRunCanceller{config: config}
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
		if asJSON || recordjson.Unmarshal(raw, &released) != nil || released.RunID == "" {
			_, err = fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
			return err
		}
		line := fmt.Sprintf("%s  released (%s)", released.RunID, released.State)
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
	if err := recordjson.Unmarshal(raw, &status); err != nil || status.Admission == nil {
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
	if _, err := fmt.Fprintf(output, "%d run(s) admitted; Argo parallelism bounds running Workflows\n", state.Runs); err != nil {
		return err
	}
	if line := admissionHoldLine(state.Hold); line != "" {
		if _, err := fmt.Fprintln(output, "hold: "+line); err != nil {
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
		if _, err := fmt.Fprintln(output, line); err != nil {
			return err
		}
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "RUN\tSTATE\tREPO/REF\tFOR")
	for _, reservation := range state.Reservations {
		age := "-"
		if !reservation.Since.IsZero() {
			age = now.Sub(reservation.Since).Truncate(time.Second).String()
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s/%s\t%s\n", reservation.RunID, reservation.State,
			reservation.Repo, reservation.Ref, age)
	}
	return writer.Flush()
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
