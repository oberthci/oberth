package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/recordjson"
	"github.com/oberthci/oberth/internal/runlog"
)

type remoteRun struct {
	// ResourceMessage is advice, separate from the exact status vocabulary.
	ResourceMessage string `json:"resource_message"`

	ID string
	// QueueSequence orders runs; the oldest one on a page is the cursor for
	// the next, older page. A scoped uplink gets none and pages by run ID
	// instead (#073).
	QueueSequence int64
	RepoID        int64
	Ref           string
	SHA           string
	Actor         string
	Trigger       string
	Status        string
	DisplayStatus string `json:"display_status"`
	DisplayReason string `json:"display_reason"`
	Scheduling    *model.RunScheduling
	Release       bool
	SupersededBy  string
	// Phase and Error carry why a run failed. Without them the client decodes
	// a failed run into a struct that says only "failed", which is the one
	// thing the reader already knows.
	Phase       string
	Error       string
	Reason      string
	FailedStage string `json:"FailedBurn"`
	FailedStep  string
	QueuedAt    time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
}

type remoteStep struct {
	Stage      string `json:"Burn"`
	Step       string
	Status     string
	StartedAt  *time.Time
	FinishedAt *time.Time
}

type remoteRepository struct {
	ID            int64
	Name          string
	DefaultBranch string
}

type remoteRunDetail struct {
	Run        remoteRun
	Steps      []remoteStep
	Repository remoteRepository
	Scheduling *model.RunScheduling
	RerunOf    string
}

func reportMode(mode string) {
	if mode == "server" {
		return
	}
	fmt.Fprintf(os.Stderr, "reading: %s\n", mode)
}

// remoteSubcommands lists the verbs a command dispatches before parsing its
// own flags, so --help shows every form the command accepts.
var remoteSubcommands = map[string]string{
	"run":  "       oberth run wait|resources|retry|rerun|cancel|purge|hold ... (see oberth run <subcommand> --help)",
	"runs": "       oberth runs cancel --repo <repo> --ref <branch> [flags] (see oberth runs cancel --help)",
}

func setRemoteUsage(flags *flag.FlagSet) {
	suffix := "[flags]"
	switch flags.Name() {
	case "run":
		suffix = "<run-id> [flags]"
	case "log":
		suffix = "<run-id> [--burn <burn> --step <step>] [flags]"
	case "issues update":
		suffix = "--id <issue-id> --work-status <status> [flags]"
	}
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "Usage: oberth %s %s\n", flags.Name(), suffix)
		if subcommands := remoteSubcommands[flags.Name()]; subcommands != "" {
			_, _ = fmt.Fprintln(flags.Output(), subcommands)
		}
		if flags.Name() == "log" {
			_, _ = fmt.Fprintln(flags.Output(), "With no pattern, limit, tail, offset or byte-offset: last 200 lines. Pass --tail --limit N for a larger tail, or --limit 0 for the existing bounded full read.")
		}
		flags.PrintDefaults()
	}
}

func remoteFlags(name string, arguments []string, output io.Writer) (*flag.FlagSet, *bool, *client.ConnectionFlags, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	setRemoteUsage(flags)
	asJSON := flags.Bool("json", false, "emit the server's payload unchanged")
	transport := registerTransportFlags(flags)
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil, nil, nil, nil
		}
		return nil, nil, nil, usageError(err)
	}
	return flags, asJSON, transport, nil
}

func emitJSON(ctx context.Context, api *client.Client, path string, query map[string]string, output io.Writer) error {
	raw, err := api.GetRaw(ctx, path, query)
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		// Not valid JSON; pass through unchanged.
		_, writeErr := output.Write(raw)
		return writeErr
	}
	indented.WriteByte('\n')
	_, err = output.Write(indented.Bytes())
	return err
}

func runRuns(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) > 0 && arguments[0] == "cancel" {
		return runRunsCancel(ctx, arguments[1:], output)
	}
	flags := flag.NewFlagSet("runs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	setRemoteUsage(flags)
	repo := flags.String("repo", "", "only this repository; use upstream/org/name when ambiguous")
	ref := flags.String("ref", "", "only this branch or tag")
	limit := flags.Int("limit", 20, "runs per page (at most 100)")
	before := flags.String("before", "", "only runs older than this cursor (printed after a full page)")
	all := flags.Bool("all", false, "follow pages to the oldest run")
	active := flags.Bool("active", false, "only queued and running runs, in scheduling order, with queue position and wait reason")
	asJSON := flags.Bool("json", false, `emit {"runs":[...],"next_before":N}; --active emits {"runs":[...]}`)
	transport := registerTransportFlags(flags)
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return usageError(err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: runs takes no positional arguments; use --repo and --ref to filter", errUsage)
	}
	api, err := remoteClient(ctx, transport)
	if err != nil {
		return err
	}
	reportMode("server")
	if *limit <= 0 || *limit > 100 {
		return fmt.Errorf("%w: --limit must be between 1 and 100", errUsage)
	}
	if sequence, err := strconv.ParseInt(*before, 10, 64); *before != "" && err == nil && sequence < 1 {
		return fmt.Errorf("%w: --before must be a positive run sequence or a run ID", errUsage)
	}
	if *active {
		if *before != "" || *all {
			return fmt.Errorf("%w: --active is one unpaged list; it takes no --before or --all", errUsage)
		}
		// The first page of the default view carries every queued and
		// running run; the one terminal row it adds is dropped.
		var page struct {
			Runs []json.RawMessage `json:"runs"`
		}
		if err := api.Get(ctx, "/api/runs", map[string]string{"repo": *repo, "ref": *ref, "limit": "1"}, &page); err != nil {
			return err
		}
		rows := make([]json.RawMessage, 0, len(page.Runs))
		runs := make([]remoteActiveRun, 0, len(page.Runs))
		for _, row := range page.Runs {
			var run remoteActiveRun
			if err := recordjson.Unmarshal(row, &run); err != nil {
				return fmt.Errorf("decode run: %w", err)
			}
			if model.RunStatus(run.Status).Active() {
				rows, runs = append(rows, row), append(runs, run)
			}
		}
		if *asJSON {
			return emitRunPage(output, rows, "")
		}
		return renderActiveRuns(output, runs, time.Now())
	}
	query := map[string]string{"repo": *repo, "ref": *ref, "limit": fmt.Sprint(*limit)}
	if *before != "" {
		query["before"] = *before
	}
	// The server returns at most one bounded page; --all walks the cursor
	// to the oldest run instead of stopping at the newest page (#050).
	rows, next, err := collectRunPages(ctx, api, query, *all)
	if err != nil {
		return err
	}
	if *asJSON {
		return emitRunPage(output, rows, next)
	}
	runs := make([]remoteRun, len(rows))
	for i, row := range rows {
		if err := recordjson.Unmarshal(row, &runs[i]); err != nil {
			return fmt.Errorf("decode run: %w", err)
		}
	}
	if len(runs) == 0 {
		_, err := fmt.Fprintln(output, "no runs")
		return err
	}
	if _, err := fmt.Fprintf(output, "%-34s %-10s %-9s %-8s %-30s %s\n", "RUN", "STATUS", "TRIGGER", "SHA", "REF", "REASON"); err != nil {
		return err
	}
	for _, run := range runs {
		if _, err := fmt.Fprintf(output, "%-34s %-10s %-9s %-8s %-30s %s\n",
			run.ID, displayRunStatus(run), run.Trigger, shortSHA(run.SHA), run.Ref, displayRunReason(run)); err != nil {
			return err
		}
	}
	if next != "" {
		_, err := fmt.Fprintf(output, "more: oberth runs --before %s (or --all)\n", next)
		return err
	}
	return nil
}

// collectRunPages reads one page, or every page with all, and returns the
// raw rows and the cursor for the next page ("" after the oldest run).
func collectRunPages(ctx context.Context, api *client.Client, query map[string]string, all bool) ([]json.RawMessage, string, error) {
	var rows []json.RawMessage
	next := ""
	for {
		var page struct {
			Runs       []json.RawMessage `json:"runs"`
			NextBefore int64             `json:"next_before"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := api.Get(ctx, "/api/runs", query, &page); err != nil {
			return nil, "", err
		}
		next = page.NextCursor
		if page.NextBefore > 0 {
			next = strconv.FormatInt(page.NextBefore, 10)
		}
		rows = append(rows, page.Runs...)
		if !all || next == "" {
			return rows, next, nil
		}
		query["before"] = next
	}
}

// emitRunPage writes `oberth runs --json`: the rows and the cursor of the
// next page, in the same envelope as GET /api/runs?envelope=1 and MCP
// run_list. next_before is a sequence; a scoped uplink pages by run ID in
// next_cursor. Neither field is present after the oldest run.
func emitRunPage(output io.Writer, rows []json.RawMessage, next string) error {
	if rows == nil {
		rows = []json.RawMessage{}
	}
	document := map[string]any{"runs": rows}
	if next != "" {
		if sequence, err := strconv.ParseInt(next, 10, 64); err == nil {
			document["next_before"] = sequence
		} else {
			document["next_cursor"] = next
		}
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

func displayRunReason(run remoteRun) string { return run.DisplayReason }

// remoteActiveRun is one /api/runs?view=active entry: the run, plus the
// scheduling observation of a run that has not started.
type remoteActiveRun struct {
	remoteRun
	Scheduling *model.RunScheduling
}

// renderActiveRuns prints the active list in the server's order: started
// runs with their elapsed time, then the queue with each run's position and
// why it waits. There is no size column: run limits live in Argo since
// v0.18.0 and runs carry no admission size.
func renderActiveRuns(output io.Writer, runs []remoteActiveRun, now time.Time) error {
	if len(runs) == 0 {
		_, err := fmt.Fprintln(output, "no active runs")
		return err
	}
	if _, err := fmt.Fprintf(output, "%-4s %-34s %-9s %-8s %-30s %s\n", "#", "RUN", "STATUS", "ELAPSED", "REF", "REASON"); err != nil {
		return err
	}
	for _, run := range runs {
		position, elapsed, reason := "-", "-", ""
		if displayRunStatus(run.remoteRun) == "queued" && run.Scheduling != nil && run.Scheduling.QueuePosition > 0 {
			position = strconv.FormatInt(run.Scheduling.QueuePosition, 10)
		}
		if run.StartedAt != nil {
			elapsed = now.Sub(*run.StartedAt).Truncate(time.Second).String()
		}
		// The active entry carries its observation beside the run; the
		// reason is the same display_reason every other list shows.
		row := run.remoteRun
		if row.Scheduling == nil {
			row.Scheduling = run.Scheduling
		}
		reason = displayRunReason(row)
		if _, err := fmt.Fprintf(output, "%-4s %-34s %-9s %-8s %-30s %s\n",
			position, run.ID, displayRunStatus(run.remoteRun), elapsed, run.Ref, reason); err != nil {
			return err
		}
	}
	return nil
}

// displayRunStatus accepts the server's read-side label and derives it from
// legacy fields when talking to an older server. The wire status is unchanged.
func displayRunStatus(run remoteRun) string {
	if run.DisplayStatus != "" {
		return run.DisplayStatus
	}
	if run.Status == string(model.RunInterrupted) {
		return model.PhaseCancelled
	}
	if run.Status == string(model.RunWaiting) || run.Status == string(model.RunQueued) ||
		run.Status == string(model.RunRunning) && run.StartedAt == nil {
		return string(model.RunQueued)
	}
	return run.Status
}

// resolveRemoteRunID leaves lookup and ambiguity detection to the server.
func resolveRemoteRunID(_ context.Context, _ *client.Client, given string) (string, error) {
	if len(given) > 0 && len(given) < 8 && strings.Trim(strings.ToLower(given), "0123456789abcdef") == "" {
		return "", fmt.Errorf("%w: run ID must contain 8 to 32 hexadecimal characters", errUsage)
	}
	return given, nil
}

func runRunDetail(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) > 0 && arguments[0] == "resources" {
		return runRunResources(ctx, arguments[1:], output)
	}
	if len(arguments) > 0 && arguments[0] == "purge" {
		return runRunPurge(ctx, arguments[1:], output)
	}
	if len(arguments) > 0 && arguments[0] == "retry" {
		return runRunRetry(ctx, arguments[1:], output)
	}
	if len(arguments) > 0 && arguments[0] == "cancel" {
		return runRunCancel(ctx, arguments[1:], output)
	}
	if len(arguments) > 0 && arguments[0] == "rerun" {
		return runRunRerun(ctx, arguments[1:], output)
	}
	if len(arguments) > 0 && arguments[0] == "wait" {
		return runRunWait(ctx, arguments[1:], output)
	}
	if len(arguments) > 0 && arguments[0] == "hold" {
		return runRunHold(ctx, arguments[1:], output)
	}
	flags, asJSON, transport, err := remoteFlags("run", arguments, output)
	if err != nil || flags == nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: run <run-id>", errUsage)
	}
	if err := refuseVerbAsRunID("run", flags.Arg(0), "<run-id>, or a subcommand: wait, resources, retry, rerun, cancel, purge or hold"); err != nil {
		return err
	}
	api, err := remoteClient(ctx, transport)
	if err != nil {
		return err
	}
	reportMode("server")
	runID, err := resolveRemoteRunID(ctx, api, flags.Arg(0))
	if err != nil {
		return err
	}
	path := "/api/runs/" + runID
	if *asJSON {
		return emitJSON(ctx, api, path, nil, output)
	}
	var detail remoteRunDetail
	if err := api.Get(ctx, path, nil, &detail); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "%s  %s  %s  %s\n",
		detail.Run.ID, displayRunStatus(detail.Run), shortSHA(detail.Run.SHA), detail.Run.Ref); err != nil {
		return err
	}
	if detail.Repository.Name != "" {
		if _, err := fmt.Fprintf(output, "repository: %s\n", detail.Repository.Name); err != nil {
			return err
		}
	}
	if detail.RerunOf != "" {
		if _, err := fmt.Fprintf(output, "rerun of: %s\n", detail.RerunOf); err != nil {
			return err
		}
	}
	if displayRunStatus(detail.Run) == "queued" && detail.Scheduling != nil {
		if _, err := fmt.Fprintln(output, detail.Scheduling.Message); err != nil {
			return err
		}
	}
	if err := renderExecutionClock(output, detail.Scheduling); err != nil {
		return err
	}
	if detail.Run.FailedStage != "" && detail.Run.FailedStep != "" {
		if _, err := fmt.Fprintf(output, "failed step: %s/%s · oberth log %s --burn %s --step %s\n", detail.Run.FailedStage, detail.Run.FailedStep, detail.Run.ID, detail.Run.FailedStage, detail.Run.FailedStep); err != nil {
			return err
		}
		for _, reason := range []*string{&detail.Run.DisplayReason, &detail.Run.Reason, &detail.Run.Error} {
			if strings.EqualFold(strings.TrimSpace(*reason), "Workflow failed") {
				*reason = ""
			}
		}
	}
	// The reason first, before the step list: a failed run is read to find out
	// what went wrong, and burying it under timings makes the reader scroll
	// past the answer.
	if detail.Run.ResourceMessage != "" {
		if _, err := fmt.Fprintln(output, detail.Run.ResourceMessage); err != nil {
			return err
		}
	}
	if detail.Run.DisplayReason != "" {
		if _, err := fmt.Fprintln(output, detail.Run.DisplayReason); err != nil {
			return err
		}
	}
	if detail.Run.Reason != "" && detail.Run.Reason != detail.Run.DisplayReason && detail.Run.Reason != detail.Run.Error {
		if _, err := fmt.Fprintln(output, detail.Run.Reason); err != nil {
			return err
		}
	}
	if reason := strings.TrimSpace(detail.Run.Error); reason != "" && reason != detail.Run.DisplayReason {
		switch {
		case detail.Run.Status == string(model.RunInterrupted) && detail.Run.Phase == model.PhaseCancelled:
			if _, err := fmt.Fprintln(output, reason); err != nil {
				return err
			}
		case detail.Run.Status == string(model.RunFailed):
			phase := detail.Run.Phase
			if phase == "" {
				phase = "unknown"
			}
			if _, err := fmt.Fprintf(output, "failed in %s: %s\n", phase, reason); err != nil {
				return err
			}
		default:
			if _, err := fmt.Fprintf(output, "error: %s\n", reason); err != nil {
				return err
			}
		}
	}
	if detail.Run.Actor != "" {
		if _, err := fmt.Fprintf(output, "actor: %s  trigger: %s\n", detail.Run.Actor, detail.Run.Trigger); err != nil {
			return err
		}
	}
	stageWidth, stepWidth, statusWidth := 0, 0, 0
	for _, step := range detail.Steps {
		stageWidth = max(stageWidth, len(step.Stage))
		stepWidth = max(stepWidth, len(step.Step))
		statusWidth = max(statusWidth, len(step.Status))
	}
	for _, step := range detail.Steps {
		marker := " "
		if step.Stage == detail.Run.FailedStage && step.Step == detail.Run.FailedStep {
			marker = "*"
		}
		if _, err := fmt.Fprintf(output, "%s %-*s %-*s %-*s %s\n",
			marker, stageWidth, step.Stage, stepWidth, step.Step, statusWidth, step.Status, span(step.StartedAt, step.FinishedAt)); err != nil {
			return err
		}
	}
	return nil
}

func runRunRetry(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 1 && (arguments[0] == "--help" || arguments[0] == "-h") {
		_, err := fmt.Fprintln(output, "Usage: oberth run retry <run-id> [--json] [--url <mcp-url>] [--token-file <path>] [--ca-file <path>]")
		return err
	}
	forwarded := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		// The MCP tool already returns the run as JSON. Consume the shared
		// CLI flag here instead of forwarding it as an unknown transport flag.
		if argument != "--json" && argument != "-json" {
			forwarded = append(forwarded, argument)
		}
	}
	arguments = forwarded
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		return fmt.Errorf("%w: run retry <run-id>", errUsage)
	}
	encoded, err := json.Marshal(map[string]string{"id": arguments[0]})
	if err != nil {
		return err
	}
	toolArguments := append([]string{"run_retry", string(encoded)}, arguments[1:]...)
	return runMCPCall(ctx, toolArguments, output)
}

type remoteLog struct {
	TotalScannedBytes int64  `json:"total_scanned_bytes"`
	ScanLimitBytes    int64  `json:"scan_limit_bytes"`
	RunID             string `json:"run_id"`
	Stage             string `json:"burn"`
	Step              string `json:"step"`
	Output            string `json:"output"`
	Note              string `json:"note"`
	RunError          string `json:"run_error"`
	TotalLines        int    `json:"total_lines"`
	MatchedLines      int    `json:"matched_lines"`
	ReturnedLines     int    `json:"returned_lines"`
	Truncated         bool   `json:"truncated"`
	Bytes             int64  `json:"bytes"`
	ScanComplete      bool   `json:"scan_complete"`
	BytesExact        bool   `json:"bytes_exact"`
	HasMore           bool   `json:"has_more"`
	NextByteOffset    int64  `json:"next_byte_offset"`
	WithheldLines     int    `json:"withheld_lines"`
	WithheldBytes     int64  `json:"withheld_bytes"`
	OmittedLongLines  int    `json:"omitted_long_lines"`
}

func runRemoteLog(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("log", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	setRemoteUsage(flags)
	stage := flags.String("burn", "", "burn name")
	step := flags.String("step", "", "step name")
	pattern := flags.String("pattern", "", "RE2 pattern; offset and limit then page over matches")
	context_ := flags.Int("context", 0, "lines of context around each match")
	offset := flags.Int("offset", 0, "skip this many")
	byteOffset := flags.Int64("byte-offset", 0, "absolute log cursor from next_byte_offset")
	limit := flags.Int("limit", 0, "return at most this many")
	tail := flags.Bool("tail", false, "read from the end")
	raw := flags.Bool("raw", false, "keep the [burn/step] prefix on each line and the Argo executor lines")
	asJSON := flags.Bool("json", false, "emit JSON, including the default window when used")
	transport := registerTransportFlags(flags)
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return usageError(err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: log <run-id> --burn <burn> --step <step>", errUsage)
	}
	if err := refuseVerbAsRunID("log", flags.Arg(0), "<run-id> [--burn <burn> --step <step>]"); err != nil {
		return err
	}
	api, err := remoteClient(ctx, transport)
	if err != nil {
		return err
	}
	reportMode("server")
	runID, err := resolveRemoteRunID(ctx, api, flags.Arg(0))
	if err != nil {
		return err
	}
	path := "/api/runs/" + runID + "/logs"
	query := map[string]string{"burn": *stage, "step": *step, "pattern": *pattern}
	for name, value := range map[string]int{"context": *context_, "offset": *offset, "limit": *limit} {
		if value > 0 {
			query[name] = fmt.Sprint(value)
		}
	}
	if *byteOffset < 0 {
		return fmt.Errorf("%w: --byte-offset must be nonnegative", errUsage)
	}
	defaultWindow := true
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "pattern", "limit", "tail", "offset", "byte-offset":
			defaultWindow = false
		}
		if f.Name == "byte-offset" {
			query["byte_offset"] = fmt.Sprint(*byteOffset)
		}
	})
	if *tail {
		query["tail"] = "true"
	}
	if defaultWindow {
		query["tail"], query["limit"] = "true", strconv.Itoa(runlog.DefaultWindowLines)
	}
	if *asJSON {
		if defaultWindow {
			var payload map[string]json.RawMessage
			if err := api.Get(ctx, path, query, &payload); err != nil {
				return err
			}
			payload["window"], _ = json.Marshal(runlog.DefaultWindowDescription)
			encoder := json.NewEncoder(output)
			encoder.SetIndent("", "  ")
			return encoder.Encode(payload)
		}
		return emitJSON(ctx, api, path, query, output)
	}
	var log remoteLog
	if err := api.Get(ctx, path, query, &log); err != nil {
		return err
	}
	body, hidden := log.Output, 0
	if !*raw {
		body = stripStepPrefix(body, log.Stage, log.Step)
		body, hidden = dropExecutorLines(body)
	}
	if _, err := io.WriteString(output, body); err != nil {
		return err
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		if _, err := io.WriteString(output, "\n"); err != nil {
			return err
		}
	}
	if log.Note != "" {
		if _, err := fmt.Fprintln(output, log.Note); err != nil {
			return err
		}
	}
	if log.RunError != "" {
		if _, err := fmt.Fprintln(output, "Run error: "+log.RunError); err != nil {
			return err
		}
	}
	summary := log.readSummary() + hiddenNote(hidden) + truncatedNote(log.Truncated)
	if defaultWindow {
		summary += "; " + runlog.DefaultWindowDescription
	}
	_, err = fmt.Fprintln(os.Stderr, summary)
	return err
}

func (log remoteLog) readSummary() string {
	lineQualifier, byteQualifier := "", ""
	if !log.ScanComplete {
		lineQualifier = "at least "
	}
	if !log.BytesExact {
		byteQualifier = "at least "
	}
	summary := fmt.Sprintf("%d of %s%d lines returned, %s%d matched, %s%d bytes in the step", log.ReturnedLines, lineQualifier, log.TotalLines, lineQualifier, log.MatchedLines, byteQualifier, log.Bytes)
	summary += fmt.Sprintf("; withheld %s%d lines / %s%d bytes; %d oversized lines omitted", lineQualifier, log.WithheldLines, byteQualifier, log.WithheldBytes, log.OmittedLongLines)
	if log.ScanLimitBytes > 0 {
		summary += fmt.Sprintf("; scanned %d of %d byte scan budget", log.TotalScannedBytes, log.ScanLimitBytes)
	}
	if log.HasMore {
		summary += fmt.Sprintf("; continue with byte_offset=%d (--byte-offset %d), offset=0 and the same tail/pattern/context", log.NextByteOffset, log.NextByteOffset)
	}
	return summary
}

// dropExecutorLines removes Argo executor chatter (runlog.IsExecutorNoise)
// from what a person reads, and counts it. The server's line counts still
// include those lines; --raw and --json show them.
func dropExecutorLines(body string) (string, int) {
	lines := strings.Split(body, "\n")
	kept, hidden := lines[:0], 0
	for _, line := range lines {
		if runlog.IsExecutorNoise(line) {
			hidden++
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), hidden
}

func hiddenNote(hidden int) string {
	if hidden == 0 {
		return ""
	}
	return fmt.Sprintf("; %d Argo executor line%s hidden (--raw shows them)", hidden, map[bool]string{true: "", false: "s"}[hidden == 1])
}

func truncatedNote(truncated bool) string {
	if truncated {
		return "; TRUNCATED, you did not see everything"
	}
	return ""
}

func stripStepPrefix(body, stage, step string) string {
	prefix := "[" + stage + "/" + step + "] "
	lines := strings.Split(body, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimPrefix(line, prefix)
	}
	return strings.Join(lines, "\n")
}

func runRepos(ctx context.Context, arguments []string, output io.Writer) error {
	flags, asJSON, transport, err := remoteFlags("repos", arguments, output)
	if err != nil || flags == nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: repos takes no arguments", errUsage)
	}
	api, err := remoteClient(ctx, transport)
	if err != nil {
		return err
	}
	reportMode("server")
	if *asJSON {
		return emitJSON(ctx, api, "/api/repos", nil, output)
	}
	var page struct {
		Repositories []remoteRepository `json:"repositories"`
	}
	if err := api.Get(ctx, "/api/repos", nil, &page); err != nil {
		return err
	}
	for _, repository := range page.Repositories {
		if _, err := fmt.Fprintf(output, "%-32s %s\n", repository.Name, repository.DefaultBranch); err != nil {
			return err
		}
	}
	return nil
}

type remoteHealthStatus struct {
	ClusterDetail string `json:"cluster_detail,omitempty"`
	Database      string `json:"database"`
	Upstreams     int    `json:"upstreams"`
	Repositories  int    `json:"repositories"`
	VCS           string `json:"vcs"`
	Cluster       string `json:"cluster"`
	Audit         string `json:"audit"`
	Version       string `json:"version,omitempty"`
	Admission     *struct {
		Hold *model.AdmissionHold `json:"hold,omitempty"`
	} `json:"admission,omitempty"`
}

// admissionHoldLine is the status line of an active administrator hold: the
// whole reason, who set it, when it ends and what may still pass.
func admissionHoldLine(hold *model.AdmissionHold) string {
	if hold == nil || !hold.On {
		return ""
	}
	line := "held by admin"
	if hold.Actor != "" {
		line += " " + hold.Actor
	}
	if !hold.ExpiresAt.IsZero() {
		line += " until " + hold.ExpiresAt.UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	if hold.Reason != "" {
		line += ": " + hold.Reason
	}
	allowed := "nothing"
	if len(hold.Allow) > 0 {
		allowed = strings.Join(hold.Allow, ", ")
	}
	return line + " (allowed: " + allowed + ")"
}

func runRemoteStatus(ctx context.Context, arguments []string, output io.Writer) error {
	flags, asJSON, transport, err := remoteFlags("status", arguments, output)
	if err != nil || flags == nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: status takes no arguments; for branch CI status use: oberth runs --ref <branch>", errUsage)
	}
	api, err := remoteClient(ctx, transport)
	if err != nil {
		return err
	}
	reportMode("server")
	if *asJSON {
		return emitJSON(ctx, api, "/api/status", nil, output)
	}
	var status remoteHealthStatus
	if err := api.Get(ctx, "/api/status", nil, &status); err != nil {
		return err
	}
	cluster := status.Cluster
	if status.ClusterDetail != "" {
		cluster += " (" + status.ClusterDetail + ")"
	}
	for _, row := range []struct{ label, value string }{
		{"database:", status.Database},
		{"vcs:", status.VCS},
		{"cluster:", cluster},
		{"audit:", status.Audit},
		{"version:", status.Version},
		{"upstreams:", fmt.Sprint(status.Upstreams)},
		{"repositories:", fmt.Sprint(status.Repositories)},
	} {
		if _, err := fmt.Fprintf(output, "%-14s %s\n", row.label, row.value); err != nil {
			return err
		}
	}
	if status.Admission != nil {
		if line := admissionHoldLine(status.Admission.Hold); line != "" {
			if _, err := fmt.Fprintf(output, "%-14s %s\n", "admission:", line); err != nil {
				return err
			}
		}
	}
	return nil
}

type remoteIssueSummary struct {
	WorkStatus string `json:"work_status"`
	Stale      bool   `json:"stale"`
	ID         int64
	State      string
	Kind       string
	Title      string
}

type remoteIssuePage struct {
	Issues     []remoteIssueSummary
	NextBefore int64
	Total      int64 `json:"total"`
}

// UnmarshalJSON reads the preferred snake_case envelope and falls back to the
// Go-style PascalCase names an older server sends, so the CLI reads both.
func (page *remoteIssuePage) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Issues       []remoteIssueSummary `json:"issues"`
		LegacyIssues []remoteIssueSummary `json:"Issues"`
		NextBefore   *int64               `json:"next_before"`
		LegacyNext   int64                `json:"NextBefore"`
		Total        int64                `json:"total"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	page.Issues, page.NextBefore, page.Total = wire.Issues, wire.LegacyNext, wire.Total
	if page.Issues == nil {
		page.Issues = wire.LegacyIssues
	}
	if wire.NextBefore != nil {
		page.NextBefore = *wire.NextBefore
	}
	return nil
}

// issuePageLimit is the server's page cap (internal/api issueFilter).
const issuePageLimit = 50

const issuesUsage = "Usage: oberth issues [--state open|closed|all] [--work-status <status>] [--limit N] [--before <issue-id>] [--all] [--json]\n" +
	"       oberth issues update --id <issue-id> --work-status <status> [flags]\n" +
	"Prints one page of at most 50 issues, newest first, with the matching total.\n" +
	"A full page ends with the --before value for the next page; --all follows every page."

func runIssues(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) > 0 && arguments[0] == "update" {
		return runIssueWorkStatus(ctx, arguments[1:], output)
	}
	flags := flag.NewFlagSet("issues", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(flags.Output(), issuesUsage)
		flags.PrintDefaults()
	}
	asJSON := flags.Bool("json", false, "emit the server payload (one page, or every page with --all)")
	issueState := flags.String("state", "open", "filter open, closed or all issues")
	workStatus := flags.String("work-status", "", "filter todo, active, waiting or review")
	staleAfter := flags.String("stale-after", "", "override the general stale window (duration)")
	activeStaleAfter := flags.String("active-stale-after", "", "override the active stale window (duration)")
	limit := flags.Int("limit", issuePageLimit, "issues per page (1 to 50)")
	before := flags.Int64("before", 0, "only issues older than this issue ID (printed after a full page)")
	all := flags.Bool("all", false, "follow every page to the oldest matching issue")
	transport := registerTransportFlags(flags)
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return usageError(err)
	}
	if *issueState != "open" && *issueState != "closed" && *issueState != "all" {
		return fmt.Errorf("%w: --state must be open, closed or all", errUsage)
	}
	selectedState := *issueState
	if selectedState == "all" {
		selectedState = ""
	}
	if *workStatus != "" && !model.IssueWorkStatus(*workStatus).Valid() {
		return fmt.Errorf("%w: invalid work status", errUsage)
	}
	for _, raw := range []string{*staleAfter, *activeStaleAfter} {
		if raw != "" {
			if value, err := time.ParseDuration(raw); err != nil || value <= 0 {
				return fmt.Errorf("%w: stale windows must be positive durations", errUsage)
			}
		}
	}
	if *limit < 1 || *limit > issuePageLimit {
		return fmt.Errorf("%w: --limit must be between 1 and %d", errUsage, issuePageLimit)
	}
	if *before < 0 {
		return fmt.Errorf("%w: --before must be a positive issue ID", errUsage)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: issues takes no arguments; use the dashboard for individual issue detail", errUsage)
	}
	api, err := remoteClient(ctx, transport)
	if err != nil {
		return err
	}
	reportMode("server")
	load := func(ctx context.Context, cursor int64) (json.RawMessage, error) {
		query := map[string]string{"state": selectedState, "work_status": *workStatus, "stale_after": *staleAfter, "active_stale_after": *activeStaleAfter, "limit": strconv.Itoa(*limit)}
		if cursor > 0 {
			query["before"] = strconv.FormatInt(cursor, 10)
		}
		return api.GetRaw(ctx, "/api/issues", query)
	}
	var raw json.RawMessage
	if *all {
		raw, err = collectIssuePages(ctx, *before, load)
	} else {
		raw, err = load(ctx, *before)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		var document any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err = decoder.Decode(&document); err != nil {
			return err
		}
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(document)
	}
	var page remoteIssuePage
	if err = recordjson.Unmarshal(raw, &page); err != nil {
		return err
	}
	if len(page.Issues) == 0 {
		if _, err := fmt.Fprintln(output, "no issues"); err != nil {
			return err
		}
		_, err := fmt.Fprintf(output, "0 of %d\n", page.Total)
		return err
	}
	if _, err := fmt.Fprintf(output, "%-6s %-8s %-8s %-8s %-5s %s\n", "ID", "STATE", "KIND", "WORK", "STALE", "TITLE"); err != nil {
		return err
	}
	for _, issue := range page.Issues {
		if _, err := fmt.Fprintf(output, "%-6d %-8s %-8s %-8s %-5t %s\n",
			issue.ID, issue.State, issue.Kind, issue.WorkStatus, issue.Stale, issue.Title); err != nil {
			return err
		}
	}
	total := page.Total
	if total == 0 { // A server before #1124 did not return total.
		total = int64(len(page.Issues))
	}
	if *before > 0 {
		// A later page has no absolute offset; name the cursor it continues.
		_, err = fmt.Fprintf(output, "%d older than #%d, of %d\n", len(page.Issues), *before, total)
	} else {
		_, err = fmt.Fprintf(output, "1–%d of %d\n", len(page.Issues), total)
	}
	if err != nil || *all || page.NextBefore <= 0 {
		return err
	}
	_, err = fmt.Fprintf(output, "more: oberth issues%s --before %d (or --all)\n", issueFilterArguments(*issueState, *workStatus, *limit), page.NextBefore)
	return err
}

// issueFilterArguments repeats the non-default filters in the next-page hint,
// so the printed command continues the same listing.
func issueFilterArguments(state, workStatus string, limit int) string {
	var builder strings.Builder
	if state != "open" {
		builder.WriteString(" --state " + state)
	}
	if workStatus != "" {
		builder.WriteString(" --work-status " + workStatus)
	}
	if limit != issuePageLimit {
		builder.WriteString(" --limit " + strconv.Itoa(limit))
	}
	return builder.String()
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func span(started, finished *time.Time) string {
	if started == nil {
		return ""
	}
	end := time.Now().UTC()
	if finished != nil {
		end = *finished
	}
	seconds := int(end.Sub(*started).Seconds())
	if seconds < 0 {
		return ""
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
}

// downloadRemoteArtifact streams a raw artifact and verifies its size and
// digest (#1011). With outputPath it requires a server-declared digest and
// renames the file into place only after verification. On stdout the bytes
// are already written when a mismatch is found, so the command still fails
// non-zero; a server without the digest header only earns a warning there.
func downloadRemoteArtifact(ctx context.Context, api *client.Client, path, outputPath string, output io.Writer) error {
	if outputPath != "" {
		var receipt client.ArtifactReceipt
		err := saveArtifactFile(outputPath, func(destination io.Writer) error {
			var downloadErr error
			receipt, downloadErr = api.DownloadArtifact(ctx, path, destination)
			if downloadErr == nil && !receipt.Verified {
				downloadErr = errUnverifiedArtifact
			}
			return downloadErr
		})
		if err != nil {
			return err
		}
		reportSavedArtifact(outputPath, receipt)
		return nil
	}
	receipt, err := api.DownloadArtifact(ctx, path, output)
	if err != nil {
		return err
	}
	if !receipt.Verified {
		fmt.Fprintf(os.Stderr, "warning: %v\n", errUnverifiedArtifact)
	}
	return nil
}

type remoteArtifactEntry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type remoteArtifactList struct {
	RunID     string                `json:"run_id"`
	Artifacts []remoteArtifactEntry `json:"artifacts"`
}

func remoteArtifacts(ctx context.Context, config client.Config, rest []string, outputPath string, output io.Writer) error {
	api, err := client.New(ctx, config)
	if err != nil {
		return err
	}
	reportMode("server")
	runID, err := resolveRemoteRunID(ctx, api, rest[0])
	if err != nil {
		return err
	}
	if len(rest) == 2 {
		return downloadRemoteArtifact(ctx, api, "/api/runs/"+runID+"/artifacts/"+rest[1], outputPath, output)
	}
	var listing remoteArtifactList
	if err := api.Get(ctx, "/api/runs/"+runID+"/artifacts", nil, &listing); err != nil {
		return err
	}
	if len(listing.Artifacts) == 0 {
		_, err := fmt.Fprintf(output, "run %s kept no artifacts\n", runID)
		return err
	}
	if _, err := fmt.Fprintf(output, "%-12s  %-20s  %s\n", "SIZE", "MODIFIED", "NAME"); err != nil {
		return err
	}
	for _, entry := range listing.Artifacts {
		if _, err := fmt.Fprintf(output, "%-12d  %-20s  %s\n",
			entry.Size, entry.Modified.UTC().Format("2006-01-02 15:04:05"), entry.Name); err != nil {
			return err
		}
	}
	return nil
}

func renderExecutionClock(output io.Writer, scheduling *model.RunScheduling) error {
	if scheduling == nil || scheduling.Execution == nil {
		return nil
	}
	execution := scheduling.Execution
	if deadline := execution.Deadline; deadline != nil {
		if deadline.ExpiresAt != nil && deadline.RemainingSeconds != nil {
			if _, err := fmt.Fprintf(output, "workflow deadline: %s; %ds remaining (clock: Workflow start)\n", deadline.ExpiresAt.UTC().Format(time.RFC3339), *deadline.RemainingSeconds); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(output, "workflow deadline: %ds budget; Workflow start not observed\n", deadline.BudgetSeconds); err != nil {
				return err
			}
		}
	}
	if execution.ReconciliationLagSeconds != nil {
		_, err := fmt.Fprintf(output, "execution observed %s; finalization lag: %ds\n", execution.Phase, *execution.ReconciliationLagSeconds)
		return err
	}
	return nil
}

// runIssueWorkStatus uses the same authenticated, audited mutation path as MCP.
func runIssueWorkStatus(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("issues update", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	setRemoteUsage(flags)
	id := flags.Int64("id", 0, "issue ID")
	status := flags.String("work-status", "", "todo, active, waiting or review")
	revision := flags.Int64("expected-revision", 0, "optional current metadata revision")
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return usageError(err)
	}
	revisionProvided := false
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "expected-revision" {
			revisionProvided = true
		}
	})
	if flags.NArg() != 0 || *id <= 0 || !model.IssueWorkStatus(*status).Valid() || (revisionProvided && *revision <= 0) {
		return fmt.Errorf("%w: issues update --id ID --work-status todo|active|waiting|review [--expected-revision N]", errUsage)
	}
	args := map[string]any{"id": *id, "work_status": *status}
	if *revision > 0 {
		args["expected_revision"] = *revision
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return runMCPCall(ctx, []string{"issue_update", string(encoded)}, output)
}
