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
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/client"
)

type remoteRun struct {
	ID      string
	RepoID  int64
	Ref     string
	SHA     string
	Actor   string
	Trigger string
	Status  string
	// Phase and Error carry why a run failed. Without them the client decodes
	// a failed run into a struct that says only "failed", which is the one
	// thing the reader already knows.
	Phase      string
	Error      string
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
	return writeIndentedJSON(raw, output)
}

func writeIndentedJSON(raw []byte, output io.Writer) error {
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		// Not valid JSON; pass through unchanged.
		_, writeErr := output.Write(raw)
		return writeErr
	}
	indented.WriteByte('\n')
	_, err := output.Write(indented.Bytes())
	return err
}

func runRuns(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("runs", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repo := flags.String("repo", "", "only this repository")
	ref := flags.String("ref", "", "only this branch or tag")
	limit := flags.Int("limit", 20, "how many runs")
	asJSON := flags.Bool("json", false, "emit the server's payload unchanged")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	api, err := remoteClient(ctx)
	if err != nil {
		return err
	}
	reportMode("server")
	query := map[string]string{"repo": *repo, "ref": *ref, "limit": fmt.Sprint(*limit)}
	if *asJSON {
		return emitJSON(ctx, api, "/api/runs", query, output)
	}
	var runs []remoteRun
	if err := api.Get(ctx, "/api/runs", query, &runs); err != nil {
		return err
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
			run.ID, run.Status, run.Trigger, shortSHA(run.SHA), run.Ref); err != nil {
			return err
		}
	}
	return nil
}

// resolveRemoteRunID expands the abbreviation the push banner prints into the
// identifier the API indexes by. Only a value that looks abbreviated costs a
// request; a whole identifier goes straight through.
func resolveRemoteRunID(ctx context.Context, api *client.Client, given string) (string, error) {
	if !looksAbbreviated(given) {
		return given, nil
	}
	var runs []remoteRun
	if err := api.Get(ctx, "/api/runs", map[string]string{"limit": "200"}, &runs); err != nil {
		return "", err
	}
	known := make([]string, 0, len(runs))
	for _, run := range runs {
		known = append(known, run.ID)
	}
	return resolveRunIDPrefix(given, known)
}

func runRunDetail(ctx context.Context, arguments []string, output io.Writer) error {
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
		detail.Run.ID, detail.Run.Status, shortSHA(detail.Run.SHA), detail.Run.Ref); err != nil {
		return err
	}
	if detail.Repository.Name != "" {
		if _, err := fmt.Fprintf(output, "repository: %s\n", detail.Repository.Name); err != nil {
			return err
		}
	}
	// The reason first, before the step list: a failed run is read to find out
	// what went wrong, and burying it under timings makes the reader scroll
	// past the answer.
	if reason := strings.TrimSpace(detail.Run.Error); reason != "" {
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
	raw := flags.Bool("raw", false, "keep the [burn/step] prefix on each line")
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
	path := "/api/runs/" + flags.Arg(0) + "/logs"
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
	body := log.Output
	if !*raw {
		body = stripStepPrefix(body, log.Burn, log.Step)
	}
	if _, err := io.WriteString(output, body); err != nil {
		return err
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		if _, err := io.WriteString(output, "\n"); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(os.Stderr, "%d of %d lines returned, %d matched, %d bytes in the step%s\n",
		log.ReturnedLines, log.TotalLines, log.MatchedLines, log.Bytes, truncatedNote(log.Truncated))
	return err
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
	// Status is where the two versions are already both in hand, and it is the
	// command someone runs when something is behaving oddly. An older CLI
	// against a newer server is a real cause of that, and it was invisible:
	// the server's version was printed and this binary's own was not anywhere
	// near it.
	warnVersionDrift(output, version, status.Version)
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

type remoteArtifactEntry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type remoteArtifactList struct {
	RunID     string                `json:"run_id"`
	Artifacts []remoteArtifactEntry `json:"artifacts"`
}

func remoteArtifacts(ctx context.Context, config client.Config, rest []string, output io.Writer) error {
	api, err := client.New(ctx, config)
	if err != nil {
		return err
	}
	reportMode("server")
	if len(rest) == 2 {
		return api.GetTo(ctx, "/api/runs/"+rest[0]+"/artifacts/"+rest[1], nil, output)
	}
	var listing remoteArtifactList
	if err := api.Get(ctx, "/api/runs/"+rest[0]+"/artifacts", nil, &listing); err != nil {
		return err
	}
	if len(listing.Artifacts) == 0 {
		_, err := fmt.Fprintf(output, "run %s kept no artifacts\n", rest[0])
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
