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

func remoteClient(ctx context.Context) (*client.Client, error) {
	config := client.FromEnv()
	if !config.Configured() {
		return nil, errors.New("set OBERTH_BASE_URL to the server's address to read it from here")
	}
	return client.New(ctx, config)
}

func reportMode(mode string) {
	if mode == "server" {
		return
	}
	fmt.Fprintf(os.Stderr, "reading: %s\n", mode)
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
		if flags.Name() == "log" {
			_, _ = fmt.Fprintln(flags.Output(), "With no pattern, limit, tail, offset or byte-offset: last 200 lines. Pass --tail --limit N for a larger tail, or --limit 0 for the existing bounded full read.")
		}
		flags.PrintDefaults()
	}
}

func remoteFlags(name string, arguments []string, output io.Writer) (*flag.FlagSet, *bool, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	setRemoteUsage(flags)
	asJSON := flags.Bool("json", false, "emit the server's payload unchanged")
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("%w: %w", errUsage, err)
	}
	return flags, asJSON, nil
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
	asJSON := flags.Bool("json", false, "emit the server's payload unchanged")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: runs takes no positional arguments; use --repo and --ref to filter", errUsage)
	}
	api, err := remoteClient(ctx)
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
		query := map[string]string{"repo": *repo, "ref": *ref, "view": "active"}
		if *asJSON {
			return emitJSON(ctx, api, "/api/runs", query, output)
		}
		var runs []remoteActiveRun
		if err := api.Get(ctx, "/api/runs", query, &runs); err != nil {
			return err
		}
		return renderActiveRuns(output, runs, time.Now())
	}
	query := map[string]string{"repo": *repo, "ref": *ref, "limit": fmt.Sprint(*limit)}
	if *before != "" {
		query["before"] = *before
	}
	if *asJSON {
		return emitJSON(ctx, api, "/api/runs", query, output)
	}
	// The server returns at most one bounded page; --all walks the cursor
	// to the oldest run instead of stopping at the newest page (#050).
	var runs []remoteRun
	next := ""
	for {
		var page []remoteRun
		headers, err := api.GetWithHeaders(ctx, "/api/runs", query, &page)
		if err != nil {
			return err
		}
		runs = append(runs, page...)
		previous := next
		next = headers.Get("X-Oberth-Next-Before")
		if next == "" && len(page) > 0 && datedRunCount(page) == *limit {
			next = runCursor(page[len(page)-1])
		}
		// A server older than paging ignores ?before and returns the newest
		// page again: a cursor that does not move backwards ends the walk
		// instead of looping forever.
		if previous != "" && !cursorMovedBack(previous, next) {
			runs = runs[:len(runs)-len(page)]
			next = ""
			break
		}
		if !*all || next == "" {
			break
		}
		query["before"] = next
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

// datedRunCount counts the rows that count against the page limit: terminal
// runs only, as the server's RunPageFull does. Queued and running rows ride
// along on the first page and never make it full.
func datedRunCount(runs []remoteRun) int {
	count := 0
	for _, run := range runs {
		if run.Status == "passed" || run.Status == "failed" || run.Status == "interrupted" {
			count++
		}
	}
	return count
}

func displayRunReason(run remoteRun) string { return run.DisplayReason }

// remoteActiveRun is one /api/runs?view=active entry: the run, plus the
// scheduling observation of a run that has not started.
type remoteActiveRun struct {
	remoteRun
	Scheduling *model.RunScheduling
}

// renderActiveRuns prints the active list in the server's order: started
// runs with their elapsed time, then the queue with each run's position,
// size and weight, and why it waits.
func renderActiveRuns(output io.Writer, runs []remoteActiveRun, now time.Time) error {
	if len(runs) == 0 {
		_, err := fmt.Fprintln(output, "no active runs")
		return err
	}
	if _, err := fmt.Fprintf(output, "%-4s %-34s %-9s %-8s %-8s %-30s %s\n", "#", "RUN", "STATUS", "SIZE", "ELAPSED", "REF", "REASON"); err != nil {
		return err
	}
	for _, run := range runs {
		position, size, elapsed, reason := "-", "-", "-", ""
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
		if scheduling := run.Scheduling; scheduling != nil {
			if admission := scheduling.Admission; admission != nil {
				if admission.Size != "" {
					size = fmt.Sprintf("%s/%d", admission.Size, admission.Weight)
				}
			}
		}
		if _, err := fmt.Fprintf(output, "%-4s %-34s %-9s %-8s %-8s %-30s %s\n",
			position, run.ID, displayRunStatus(run.remoteRun), size, elapsed, run.Ref, reason); err != nil {
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

// runCursor is the before value for the page after run: its sequence, or its
// ID when the server withholds sequences from a scoped uplink (#073).
func runCursor(run remoteRun) string {
	if run.QueueSequence > 0 {
		return strconv.FormatInt(run.QueueSequence, 10)
	}
	return run.ID
}

// cursorMovedBack accepts a different cursor as progress. The default list
// pages terminal runs by end time, so its queue sequence can increase across
// older pages; a repeated cursor still detects an older server without paging.
func cursorMovedBack(previous, next string) bool {
	return next != previous
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
	if len(arguments) > 0 && arguments[0] == "hold" {
		return runRunHold(ctx, arguments[1:], output)
	}
	flags, asJSON, err := remoteFlags("run", arguments, output)
	if err != nil || flags == nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: run <run-id>", errUsage)
	}
	api, err := remoteClient(ctx)
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
		if admission := detail.Scheduling.Admission; admission != nil && admission.Weight > 0 {
			if _, err := fmt.Fprintf(output, "capacity: need %d weight, %d/%d used, queue position %d\n",
				admission.Weight, admission.Used, admission.Capacity, admission.Position); err != nil {
				return err
			}
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
	ScanComplete      *bool  `json:"scan_complete"`
	BytesExact        *bool  `json:"bytes_exact"`
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
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: log <run-id> --burn <burn> --step <step>", errUsage)
	}
	api, err := remoteClient(ctx)
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

// Missing completeness flags preserve compatibility with older servers.
func (log remoteLog) readSummary() string {
	lineQualifier, byteQualifier := "", ""
	if log.ScanComplete != nil && !*log.ScanComplete {
		lineQualifier = "at least "
	}
	if log.BytesExact != nil && !*log.BytesExact {
		byteQualifier = "at least "
	}
	summary := fmt.Sprintf("%d of %s%d lines returned, %s%d matched, %s%d bytes in the step", log.ReturnedLines, lineQualifier, log.TotalLines, lineQualifier, log.MatchedLines, byteQualifier, log.Bytes)
	if log.ScanComplete != nil {
		summary += fmt.Sprintf("; withheld %s%d lines / %s%d bytes; %d oversized lines omitted", lineQualifier, log.WithheldLines, byteQualifier, log.WithheldBytes, log.OmittedLongLines)
	}
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
	flags, asJSON, err := remoteFlags("repos", arguments, output)
	if err != nil || flags == nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: repos takes no arguments", errUsage)
	}
	api, err := remoteClient(ctx)
	if err != nil {
		return err
	}
	reportMode("server")
	if *asJSON {
		return emitJSON(ctx, api, "/api/repos", nil, output)
	}
	var repositories []remoteRepository
	if err := api.Get(ctx, "/api/repos", nil, &repositories); err != nil {
		return err
	}
	for _, repository := range repositories {
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
}

func runRemoteStatus(ctx context.Context, arguments []string, output io.Writer) error {
	flags, asJSON, err := remoteFlags("status", arguments, output)
	if err != nil || flags == nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: status takes no arguments; for branch CI status use: oberth runs --ref <branch>", errUsage)
	}
	api, err := remoteClient(ctx)
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

func runIssues(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) > 0 && arguments[0] == "update" {
		return runIssueWorkStatus(ctx, arguments[1:], output)
	}
	flags := flag.NewFlagSet("issues", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	setRemoteUsage(flags)
	asJSON := flags.Bool("json", false, "emit the server payload")
	issueState := flags.String("state", "open", "filter open, closed or all issues")
	workStatus := flags.String("work-status", "", "filter todo, active, waiting or review")
	staleAfter := flags.String("stale-after", "", "override the general stale window (duration)")
	activeStaleAfter := flags.String("active-stale-after", "", "override the active stale window (duration)")
	if err := parseCommandFlags(flags, arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
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
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: issues takes no arguments; use the dashboard for individual issue detail", errUsage)
	}
	api, err := remoteClient(ctx)
	if err != nil {
		return err
	}
	reportMode("server")
	raw, err := collectIssuePages(ctx, func(ctx context.Context, before int64) (json.RawMessage, error) {
		query := map[string]string{"state": selectedState, "work_status": *workStatus, "stale_after": *staleAfter, "active_stale_after": *activeStaleAfter}
		if before > 0 {
			query["before"] = strconv.FormatInt(before, 10)
		}
		return api.GetRaw(ctx, "/api/issues", query)
	})
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
	if err = json.Unmarshal(raw, &page); err != nil {
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
	_, err = fmt.Fprintf(output, "1–%d of %d\n", len(page.Issues), total)
	return err
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
		return fmt.Errorf("%w: %w", errUsage, err)
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
