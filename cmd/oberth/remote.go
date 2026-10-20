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
	SupersededBy  string
	// Phase and Error carry why a run failed. Without them the client decodes
	// a failed run into a struct that says only "failed", which is the one
	// thing the reader already knows.
	Phase      string
	Error      string
	Reason     string
	FailedBurn string
	FailedStep string
	QueuedAt   time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

type remoteStep struct {
	Burn       string
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
}

func remoteClient(ctx context.Context) (*client.Client, error) {
	config := client.FromEnv()
	if !config.Configured() {
		return nil, errors.New("set OBERTH_BASE_URL to the server's address to read it from here")
	}
	return client.New(ctx, config)
}

func reportMode(mode string) {
	fmt.Fprintf(os.Stderr, "reading: %s\n", mode)
}

func remoteFlags(name string, arguments []string, output io.Writer) (*flag.FlagSet, *bool, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	asJSON := flags.Bool("json", false, "emit the server's payload unchanged")
	if err := flags.Parse(permuteFlagsFirst(arguments)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("%w: %w", errUsage, err)
	}
	return flags, asJSON, nil
}

// permuteFlagsFirst moves flags ahead of positional arguments.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `oberth run <id> --json` reads --json as a second positional and fails with
// a usage error that says nothing about flag order. The documentation promises
// --json on every command without qualifying where it goes, and the natural
// place to type it is at the end.
//
// Everything after a bare "--" is left alone, so a positional that begins with
// a dash can still be passed.
func permuteFlagsFirst(arguments []string) []string {
	var flagsSeen, positional []string
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			positional = append(positional, arguments[index+1:]...)
			break
		}
		if strings.HasPrefix(argument, "-") && argument != "-" {
			flagsSeen = append(flagsSeen, argument)
			// A flag that takes a value consumes the next argument, unless the
			// value was already joined with "=".
			if !strings.Contains(argument, "=") && index+1 < len(arguments) &&
				flagTakesValue(argument) {
				index++
				flagsSeen = append(flagsSeen, arguments[index])
			}
			continue
		}
		positional = append(positional, argument)
	}
	return append(flagsSeen, positional...)
}

// flagTakesValue reports whether a remote-command flag consumes the argument
// after it. Booleans must not, or they would swallow the run ID.
func flagTakesValue(argument string) bool {
	name := strings.TrimLeft(argument, "-")
	switch name {
	case "json", "tail", "raw":
		return false
	default:
		return true
	}
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
	repo := flags.String("repo", "", "only this repository")
	ref := flags.String("ref", "", "only this branch or tag")
	limit := flags.Int("limit", 20, "runs per page (at most 100)")
	before := flags.String("before", "", "only runs older than this cursor (printed after a full page)")
	all := flags.Bool("all", false, "follow pages to the oldest run")
	active := flags.Bool("active", false, "only queued, waiting and running runs, in scheduling order, with queue position and wait reason")
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
		if err := api.Get(ctx, "/api/runs", query, &page); err != nil {
			return err
		}
		runs = append(runs, page...)
		previous := next
		next = ""
		if len(page) == *limit {
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
	if _, err := fmt.Fprintf(output, "%-34s %-9s %-9s %-8s %s\n", "RUN", "STATUS", "TRIGGER", "SHA", "REF"); err != nil {
		return err
	}
	for _, run := range runs {
		if _, err := fmt.Fprintf(output, "%-34s %-9s %-9s %-8s %s\n",
			run.ID, displayRunStatus(run), run.Trigger, shortSHA(run.SHA), run.Ref); err != nil {
			return err
		}
	}
	if next != "" {
		_, err := fmt.Fprintf(output, "more: oberth runs --before %s (or --all)\n", next)
		return err
	}
	return nil
}

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
		if run.StartedAt != nil {
			elapsed = now.Sub(*run.StartedAt).Truncate(time.Second).String()
		}
		if scheduling := run.Scheduling; scheduling != nil {
			reason = scheduling.Message
			if scheduling.QueuePosition > 0 {
				position = "q" + strconv.FormatInt(scheduling.QueuePosition, 10)
			}
			if admission := scheduling.Admission; admission != nil {
				if admission.Position > 0 {
					position = strconv.Itoa(admission.Position)
				}
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
	if run.Status == string(model.RunInterrupted) && run.SupersededBy != "" {
		return "superseded"
	}
	if run.Status == string(model.RunInterrupted) && run.Phase == model.PhaseCancelled {
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

// cursorMovedBack reports whether next pages further back than previous: a
// smaller sequence, or a different run ID (IDs carry no order).
func cursorMovedBack(previous, next string) bool {
	if next == "" {
		return true
	}
	before, previousErr := strconv.ParseInt(previous, 10, 64)
	after, nextErr := strconv.ParseInt(next, 10, 64)
	if previousErr == nil && nextErr == nil {
		return after < before
	}
	return next != previous
}

// resolveRemoteRunID expands the abbreviation the push banner prints into the
// identifier the API indexes by. Only a value that looks abbreviated costs a
// request; a whole identifier goes straight through.
func resolveRemoteRunID(ctx context.Context, api *client.Client, given string) (string, error) {
	if !looksAbbreviated(given) {
		return given, nil
	}
	// Walk every page: the server returns at most 100 runs per request, so
	// a short ID from an older run was "not found" (#050). Bounded so a
	// misbehaving server cannot loop forever.
	var known []string
	query := map[string]string{"limit": "100"}
	for page := 0; page < 1000; page++ {
		var runs []remoteRun
		if err := api.Get(ctx, "/api/runs", query, &runs); err != nil {
			return "", err
		}
		cursor := ""
		if len(runs) > 0 {
			cursor = runCursor(runs[len(runs)-1])
		}
		// An older server ignores ?before and repeats the newest page; stop
		// before counting the same runs twice (each would then "match" twice).
		if previous := query["before"]; previous != "" && !cursorMovedBack(previous, cursor) {
			break
		}
		for _, run := range runs {
			known = append(known, run.ID)
		}
		if len(runs) < 100 || cursor == "" {
			break
		}
		query["before"] = cursor
	}
	return resolveRunIDPrefix(given, known)
}

func runRunDetail(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) > 0 && arguments[0] == "retry" {
		return runRunRetry(ctx, arguments[1:], output)
	}
	if len(arguments) > 0 && arguments[0] == "cancel" {
		return runRunCancel(ctx, arguments[1:], output)
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
	// The reason first, before the step list: a failed run is read to find out
	// what went wrong, and burying it under timings makes the reader scroll
	// past the answer.
	if detail.Run.SupersededBy != "" {
		if _, err := fmt.Fprintf(output, "superseded by %s (oberth run %s)\n", detail.Run.SupersededBy, detail.Run.SupersededBy); err != nil {
			return err
		}
	} else if detail.Run.Status == string(model.RunInterrupted) && detail.Run.Phase == model.PhaseCancelled {
		if _, err := fmt.Fprintln(output, detail.Run.Reason); err != nil {
			return err
		}
	} else if reason := strings.TrimSpace(detail.Run.Error); reason != "" {
		phase := detail.Run.Phase
		if phase == "" {
			phase = "unknown"
		}
		if _, err := fmt.Fprintf(output, "failed in %s: %s\n", phase, reason); err != nil {
			return err
		}
	}
	if detail.Run.Actor != "" {
		if _, err := fmt.Fprintf(output, "actor: %s  trigger: %s\n", detail.Run.Actor, detail.Run.Trigger); err != nil {
			return err
		}
	}
	for _, step := range detail.Steps {
		marker := " "
		if step.Burn == detail.Run.FailedBurn && step.Step == detail.Run.FailedStep {
			marker = "*"
		}
		if _, err := fmt.Fprintf(output, "%s %-12s %-20s %-9s %s\n",
			marker, step.Burn, step.Step, step.Status, span(step.StartedAt, step.FinishedAt)); err != nil {
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
	RunID         string `json:"run_id"`
	Burn          string `json:"burn"`
	Step          string `json:"step"`
	Output        string `json:"output"`
	TotalLines    int    `json:"total_lines"`
	MatchedLines  int    `json:"matched_lines"`
	ReturnedLines int    `json:"returned_lines"`
	Truncated     bool   `json:"truncated"`
	Bytes         int64  `json:"bytes"`
}

func runRemoteLog(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("log", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	burn := flags.String("burn", "", "burn name")
	step := flags.String("step", "", "step name")
	pattern := flags.String("pattern", "", "RE2 pattern; offset and limit then page over matches")
	context_ := flags.Int("context", 0, "lines of context around each match")
	offset := flags.Int("offset", 0, "skip this many")
	limit := flags.Int("limit", 0, "return at most this many")
	tail := flags.Bool("tail", false, "read from the end")
	raw := flags.Bool("raw", false, "keep the [burn/step] prefix on each line and the Argo executor lines")
	asJSON := flags.Bool("json", false, "emit the server's payload unchanged")
	if err := flags.Parse(permuteFlagsFirst(arguments)); err != nil {
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
	if strings.TrimSpace(*burn) == "" || strings.TrimSpace(*step) == "" {
		return fmt.Errorf("%w: --burn and --step are required; oberth run <id> lists them", errUsage)
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
	query := map[string]string{"burn": *burn, "step": *step, "pattern": *pattern}
	for name, value := range map[string]int{"context": *context_, "offset": *offset, "limit": *limit} {
		if value > 0 {
			query[name] = fmt.Sprint(value)
		}
	}
	if *tail {
		query["tail"] = "true"
	}
	if *asJSON {
		return emitJSON(ctx, api, path, query, output)
	}
	var log remoteLog
	if err := api.Get(ctx, path, query, &log); err != nil {
		return err
	}
	body, hidden := log.Output, 0
	if !*raw {
		body = stripStepPrefix(body, log.Burn, log.Step)
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
	_, err = fmt.Fprintf(os.Stderr, "%d of %d lines returned, %d matched, %d bytes in the step%s%s\n",
		log.ReturnedLines, log.TotalLines, log.MatchedLines, log.Bytes, hiddenNote(hidden), truncatedNote(log.Truncated))
	return err
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

func stripStepPrefix(body, burn, step string) string {
	prefix := "[" + burn + "/" + step + "] "
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
	Database     string `json:"database"`
	Upstreams    int    `json:"upstreams"`
	Repositories int    `json:"repositories"`
	VCS          string `json:"vcs"`
	Cluster      string `json:"cluster"`
	Audit        string `json:"audit"`
	Version      string `json:"version,omitempty"`
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
	for _, row := range []struct{ label, value string }{
		{"database:", status.Database},
		{"vcs:", status.VCS},
		{"cluster:", status.Cluster},
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
	ID    int64
	State string
	Kind  string
	Title string
}

type remoteIssuePage struct {
	Issues     []remoteIssueSummary
	NextBefore int64
}

func runIssues(ctx context.Context, arguments []string, output io.Writer) error {
	flags, asJSON, err := remoteFlags("issues", arguments, output)
	if err != nil || flags == nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: issues takes no arguments; use the dashboard for individual issue detail", errUsage)
	}
	api, err := remoteClient(ctx)
	if err != nil {
		return err
	}
	reportMode("server")
	if *asJSON {
		return emitJSON(ctx, api, "/api/issues", nil, output)
	}
	var page remoteIssuePage
	if err := api.Get(ctx, "/api/issues", nil, &page); err != nil {
		return err
	}
	if len(page.Issues) == 0 {
		_, err := fmt.Fprintln(output, "no issues")
		return err
	}
	if _, err := fmt.Fprintf(output, "%-6s %-8s %-8s %s\n", "ID", "STATE", "KIND", "TITLE"); err != nil {
		return err
	}
	for _, issue := range page.Issues {
		if _, err := fmt.Fprintf(output, "%-6d %-8s %-8s %s\n",
			issue.ID, issue.State, issue.Kind, issue.Title); err != nil {
			return err
		}
	}
	return nil
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
