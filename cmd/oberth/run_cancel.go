package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

const runCancelUsage = "Usage: oberth run cancel <run-id> [--reason <text>] [--force] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>] | [--database <path>]"

const runsCancelUsage = "Usage: oberth runs cancel --repo <repo> --ref <branch> [--reason <text>] [--force] [--json]\n" +
	"         [--url <mcp-url>] [--token-file <path>] [--ca-file <path>] | [--database <path>]"

// cancelOptions are the flags shared by `run cancel` and `runs cancel`.
// Without --database both forward to the run_cancel MCP tool as the
// configured uplink; with it they act in-pod as the administrative actor on
// the live daemon's database, after its audit mutation gate.
type cancelOptions struct {
	reason, database, repo, ref string
	force, asJSON               bool
	transport                   []string
	positional                  []string
}

func parseCancelOptions(arguments []string, allowSelector bool) (cancelOptions, error) {
	var options cancelOptions
	value := func(index *int, name string) (string, error) {
		argument := arguments[*index]
		if prefix := name + "="; strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix), nil
		}
		if *index+1 >= len(arguments) {
			return "", fmt.Errorf("%w: %s requires a value", errUsage, name)
		}
		*index++
		return arguments[*index], nil
	}
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		name := ""
		if strings.HasPrefix(argument, "-") {
			name, _, _ = strings.Cut(argument, "=")
			name = "--" + strings.TrimLeft(name, "-")
		}
		var err error
		switch {
		case argument == "--":
			options.positional = append(options.positional, arguments[index+1:]...)
			index = len(arguments)
		case name == "--reason":
			options.reason, err = value(&index, "--reason")
		case name == "--database":
			options.database, err = value(&index, "--database")
		case name == "--repo" && allowSelector:
			options.repo, err = value(&index, "--repo")
		case name == "--ref" && allowSelector:
			options.ref, err = value(&index, "--ref")
		case argument == "--force" || argument == "-force":
			options.force = true
		case argument == "--json" || argument == "-json":
			options.asJSON = true
		case name == "--url" || name == "--token-file" || name == "--ca-file":
			var flagValue string
			flagValue, err = value(&index, name)
			options.transport = append(options.transport, name+"="+flagValue)
		case strings.HasPrefix(argument, "-"):
			return cancelOptions{}, fmt.Errorf("%w: unknown flag %q", errUsage, argument)
		default:
			options.positional = append(options.positional, argument)
		}
		if err != nil {
			return cancelOptions{}, err
		}
	}
	if options.database != "" && len(options.transport) > 0 {
		return cancelOptions{}, fmt.Errorf("%w: --database (in-pod) and --url/--token-file/--ca-file (remote) are exclusive", errUsage)
	}
	if err := store.ValidateCancelReason(strings.TrimSpace(options.reason)); err != nil {
		return cancelOptions{}, fmt.Errorf("%w: %w", errUsage, err)
	}
	return options, nil
}

func wantsHelp(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--help" || argument == "-h" {
			return true
		}
	}
	return false
}

// runCanceller is one transport for run_cancel: the remote MCP tool, or the
// in-pod store as the administrative actor.
type runCanceller interface {
	cancel(ctx context.Context, id, reason string, force bool) (api.RunCancelResponse, json.RawMessage, error)
	activeRuns(ctx context.Context, repo, ref string) ([]string, error)
	close()
}

func newRunCanceller(ctx context.Context, options cancelOptions) (runCanceller, error) {
	if options.database == "" {
		config, err := parseMCPFlags("run cancel", options.transport, 0)
		if err != nil {
			return nil, err
		}
		return remoteRunCanceller{config: config}, nil
	}
	if err := requestLiveAdminMutationGate(ctx, "run.cancel", options.database); err != nil {
		return nil, err
	}
	database, err := store.OpenAdminClient(ctx, options.database, configuredStoreOptions(ctx))
	if err != nil {
		return nil, err
	}
	return &podRunCanceller{database: database, path: options.database, socket: defaultAdminSocket}, nil
}

func runRunCancel(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, runCancelUsage)
		return err
	}
	options, err := parseCancelOptions(arguments, false)
	if err != nil {
		return err
	}
	if len(options.positional) != 1 {
		return fmt.Errorf("%w: run cancel <run-id>", errUsage)
	}
	canceller, err := newRunCanceller(ctx, options)
	if err != nil {
		return err
	}
	defer canceller.close()
	response, raw, err := canceller.cancel(ctx, options.positional[0], options.reason, options.force)
	if err != nil {
		return err
	}
	return writeRunCancel(output, response, raw, options.asJSON)
}

// runRunsCancel cancels every queued, waiting or running run of one ref.
// Each run is cancelled independently under the same rules as run cancel; a
// refusal of one does not stop the others, and is reported and returned.
func runRunsCancel(ctx context.Context, arguments []string, output io.Writer) error {
	if wantsHelp(arguments) {
		_, err := fmt.Fprintln(output, runsCancelUsage)
		return err
	}
	options, err := parseCancelOptions(arguments, true)
	if err != nil {
		return err
	}
	if len(options.positional) != 0 || strings.TrimSpace(options.repo) == "" || strings.TrimSpace(options.ref) == "" {
		return fmt.Errorf("%w: runs cancel --repo <repo> --ref <branch>", errUsage)
	}
	canceller, err := newRunCanceller(ctx, options)
	if err != nil {
		return err
	}
	defer canceller.close()
	ids, err := canceller.activeRuns(ctx, options.repo, options.ref)
	if err != nil {
		return err
	}
	if len(ids) == 0 && !options.asJSON {
		_, err := fmt.Fprintf(output, "no active runs on %s %s\n", options.repo, options.ref)
		return err
	}
	var failures []error
	results := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		response, raw, err := canceller.cancel(ctx, id, options.reason, options.force)
		if err != nil {
			failures = append(failures, fmt.Errorf("run %s: %w", id, err))
			if !options.asJSON {
				if _, writeErr := fmt.Fprintf(output, "%s  not cancelled: %v\n", id, err); writeErr != nil {
					return writeErr
				}
			}
			continue
		}
		if options.asJSON {
			results = append(results, raw)
			continue
		}
		if err := writeRunCancel(output, response, raw, false); err != nil {
			return err
		}
	}
	if options.asJSON {
		encoded, err := json.MarshalIndent(map[string]any{"cancelled": results}, "", "  ")
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(output, "%s\n", encoded); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

func writeRunCancel(output io.Writer, response api.RunCancelResponse, raw json.RawMessage, asJSON bool) error {
	if asJSON {
		_, err := fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
		return err
	}
	if response.RunID == "" {
		// Not a run_cancel record (an older server): show what it said.
		_, err := fmt.Fprintf(output, "%s\n", strings.TrimSpace(string(raw)))
		return err
	}
	line := fmt.Sprintf("%s  cancelled (was %s; workflow %s)  %s", response.RunID, response.PreviousState, response.Workflow, response.Reason)
	if response.PromotionID != "" {
		line += "\n  promotion " + response.PromotionID + " failed; nothing was merged"
	}
	if response.Warning != "" {
		line += "\n  warning: " + response.Warning
	}
	_, err := fmt.Fprintln(output, line)
	return err
}

type remoteRunCanceller struct{ config mcpFlagResult }

func (remote remoteRunCanceller) close() {}

func (remote remoteRunCanceller) call(ctx context.Context, tool string, arguments any) (json.RawMessage, error) {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	response, err := mcpRequest(ctx, remote.config, mcpMethodToolsCall, toolsCallParams{Name: tool, Arguments: encoded})
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

func (remote remoteRunCanceller) cancel(ctx context.Context, id, reason string, force bool) (api.RunCancelResponse, json.RawMessage, error) {
	arguments := map[string]any{"id": id}
	if reason = strings.TrimSpace(reason); reason != "" {
		arguments["reason"] = reason
	}
	if force {
		arguments["force"] = true
	}
	raw, err := remote.call(ctx, "run_cancel", arguments)
	if err != nil {
		return api.RunCancelResponse{}, nil, err
	}
	var response api.RunCancelResponse
	_ = json.Unmarshal(raw, &response)
	return response, raw, nil
}

func (remote remoteRunCanceller) activeRuns(ctx context.Context, repo, ref string) ([]string, error) {
	raw, err := remote.call(ctx, "run_list", map[string]any{"repo": repo, "ref": ref, "limit": 100})
	if err != nil {
		return nil, err
	}
	var page struct {
		Runs []struct {
			ID      string
			RefKind string
			Status  string
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("decode run_list: %w", err)
	}
	ids := make([]string, 0, len(page.Runs))
	for _, run := range page.Runs {
		// A tag of the same name is not the branch's work.
		if model.RunStatus(run.Status).Active() && model.RefKind(run.RefKind) == model.RefBranch {
			ids = append(ids, run.ID)
		}
	}
	return ids, nil
}

// podRunCanceller cancels in-pod as the administrative actor. It records the
// same audited cancel and obligation as the MCP tool, then asks the live
// daemon over its admin socket to run its cancellation pass, which withdraws
// a waiting run's reservation and deletes a claimed run's Workflow.
type podRunCanceller struct {
	database *store.Store
	path     string
	socket   string
}

func (pod *podRunCanceller) close() { _ = pod.database.Close() }

func (pod *podRunCanceller) cancel(ctx context.Context, id, reason string, force bool) (api.RunCancelResponse, json.RawMessage, error) {
	run, err := pod.database.ResolveRunID(ctx, id)
	if err != nil {
		return api.RunCancelResponse{}, nil, err
	}
	if !run.Status.Active() {
		return api.RunCancelResponse{}, nil, fmt.Errorf("%w: run %s is already %s; nothing to cancel", store.ErrRunNotActive, run.ID, run.Status)
	}
	var warning string
	result, err := pod.database.CancelRun(ctx, model.RunCancelRequest{
		RunID: run.ID, Actor: administrativeActor, Reason: reason, Force: force,
		Authorize: func(current model.Run) (string, error) {
			decided, err := service.RunCancelPolicy(current, true, force)
			warning = decided
			return decided, err
		},
	})
	if err != nil {
		return api.RunCancelResponse{}, nil, err
	}
	response := api.RunCancelResponse{
		RunID: result.Run.ID, Status: string(result.Run.Status), Phase: result.Run.Phase,
		PreviousState: string(result.PreviousState), CancelledBy: administrativeActor, Reason: result.Run.Reason,
		Workflow: "none", PromotionID: result.PromotionID, Warning: warning,
	}
	if len(result.Cancellations) > 0 {
		response.Workflow = "pending"
		if result.Cancellations[0].JobName == "" {
			response.Workflow = "not_submitted"
		}
		if err := requestAdminSchedulerWake(ctx, pod.socket, pod.path); err != nil {
			notice := "the live scheduler deletes the Workflow on its next cancellation pass: " + err.Error()
			if response.Warning != "" {
				notice = response.Warning + "; " + notice
			}
			response.Warning = notice
		}
	}
	raw, err := json.Marshal(response)
	return response, raw, err
}

func (pod *podRunCanceller) activeRuns(ctx context.Context, repo, ref string) ([]string, error) {
	repository, err := pod.database.RepositoryByName(ctx, repo)
	if err != nil {
		return nil, err
	}
	runs, err := pod.database.ListRecentRuns(ctx, model.RunListFilter{RepoID: repository.ID, Ref: ref, Limit: 100})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		if run.Status.Active() && run.RefKind == model.RefBranch {
			ids = append(ids, run.ID)
		}
	}
	return ids, nil
}
