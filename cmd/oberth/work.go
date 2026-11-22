package main

import (
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func promote(s *flowSession, args []string) error {
	f := flowFlags("promote", s.app.home())
	rest, err := parseFlow(f, args)
	repo := flagValue[string](f, "repo")
	if err != nil {
		return err
	}
	sha, err := s.commit(rest)
	if err != nil {
		return err
	}
	if *repo == "" {
		*repo, err = s.repository()
		if err != nil {
			return err
		}
	}
	var result apicontract.PromoteResult
	if err = s.invoke("promote", apicontract.PromoteArgs{Repo: *repo, SHA: sha}, &result); err != nil {
		return err
	}
	if err = s.fact("tested run: %s", result.TestedRunID); err != nil {
		return err
	}
	status, err := s.promotionStatus(result.ID)
	if err != nil {
		return err
	}
	if err = passedPromotion(status); err != nil {
		return err
	}
	if status.Promotion.ID != result.ID {
		if err = s.fact("retested on the moved default branch: promotion %s", status.Promotion.ID); err != nil {
			return err
		}
	}
	if err = s.fact("main commit: %s", status.Promotion.ResultSHA); err != nil {
		return err
	}
	return s.promotionCI(*repo, status.Promotion.ResultSHA)
}

func (s *flowSession) promotionCI(repo, sha string) error {
	run, err := s.waitFor(apicontract.WaitArgs{Repo: repo, SHA: sha})
	if err != nil {
		return err
	}
	if err = s.fact("CI run: %s\nresult: %s", run.ID, run.Status); err != nil {
		return err
	}
	if run.Status != stepmodel.RunPassed {
		_ = s.fact("next: oberth log %s", run.ID)
		return flowError("promotion_ci", nil)
	}
	return s.fact("next: oberth release <tag>")
}

func (s *flowSession) waitFor(args apicontract.WaitArgs) (*stepmodel.RunSummary, error) {
	for {
		var result apicontract.WaitResult
		if err := s.invoke("wait", args, &result); err != nil {
			return nil, err
		}
		if result.Run == nil && !result.TimedOut {
			return nil, flowError("contract", nil)
		}
		if result.TimedOut {
			continue
		}
		return result.Run, nil
	}
}
func release(s *flowSession, args []string) error {
	if !validFlowArgs("release", args) {
		return usageProblems{"release requires a tag"}
	}
	tag := args[0]
	if _, err := s.git("check-ref-format", "refs/tags/"+tag); err != nil {
		problem := flowError("tag_name", err)
		problem.Context = []string{"Tag: " + tag}
		return problem
	}
	repo, err := s.repository()
	if err != nil {
		return err
	}
	sha, err := s.releaseHead(repo)
	if err != nil {
		return err
	}
	if err = s.releasePreflight(repo, sha, tag); err != nil {
		return err
	}
	if err = s.ensureReleaseTag(tag, sha); err != nil {
		return err
	}
	if _, err = s.git("push", "--", "oberth", "refs/tags/"+tag+":refs/tags/"+tag); err != nil {
		return err
	}
	run, err := s.waitFor(apicontract.WaitArgs{Repo: repo, Ref: "refs/tags/" + tag, Trigger: string(model.TriggerTag)})
	if err != nil {
		return err
	}
	if err = s.fact("tagged %s at %s\nrelease run: %s\nresult: %s", tag, sha, run.ID, run.Status); err != nil {
		return err
	}
	if run.Status != stepmodel.RunPassed {
		// The forge got the tag at admission and keeps it, so the version is spent.
		problem := flowError("release_used", nil)
		problem.Context = []string{"Used version: " + tag, "Logs: oberth log " + run.ID}
		return problem
	}
	return s.releaseReceipt(run.ID)
}
func logRun(s *flowSession, args []string) error {
	f := flowFlags("log", s.app.home())
	rest, err := parseFlow(f, args)
	pattern := flagValue[string](f, "pattern")
	tail := flagValue[bool](f, "tail")
	if err != nil {
		return err
	}
	if !validFlowArgs("log", rest) {
		return usageProblems{"log requires a run and optional step"}
	}
	request := apicontract.RunLogsArgs{ID: rest[0], Tail: tail}
	if len(rest) == 2 {
		request.Step = rest[1]
	}
	if *pattern != "" {
		request.Pattern = pattern
	}
	return s.logPlannedSteps(request)
}

// promotionStatus waits for the promotion's outcome, followed by the server to
// its retest when the default branch moved. A retest still running when a
// wait times out is reported as such, so the person does not promote again.
func (s *flowSession) promotionStatus(id string) (apicontract.PromotionWaitResult, error) {
	publishing := false
	for {
		var status apicontract.PromotionWaitResult
		if err := s.invoke("promote_status", apicontract.PromoteStatusArgs{ID: id}, &status); err != nil {
			return status, err
		}
		if status.TimedOut {
			if retesting(id, status) {
				problem := flowError("retesting", nil)
				problem.Context = []string{"State: oberth promote status " + status.Promotion.ID}
				return status, problem
			}
			if p := status.Publication; p != nil && p.Status == model.PublicationPending && !publishing {
				publishing = true
				if err := s.fact("promotion passed; publishing: at most %d attempts in about %d minutes\npublication: %s", pushretry.MaxAttempts, int(publicationBudget().Minutes()), p.Reason); err != nil {
					return status, err
				}
			}
			continue
		}
		if status.Promotion == nil {
			return status, flowError("contract", nil)
		}
		if status.Promotion.Status != model.PromotionPending {
			return status, nil
		}
	}
}

// publicationBudget is how long a publication retries before it fails.
func publicationBudget() time.Duration {
	var total time.Duration
	for attempt := 1; attempt < pushretry.MaxAttempts; attempt++ {
		total += pushretry.Delay(attempt)
	}
	return total
}

// retesting reports a wait that the server answered with the retest it started
// on a moved default branch, still running its tests: a promotion is retested
// only when that retest exists, and a passed one is publishing instead.
func retesting(id string, status apicontract.PromotionWaitResult) bool {
	p := status.Promotion
	return p != nil && p.ID != id && p.Status == model.PromotionPending
}

func (s *flowSession) releaseHead(repo string) (string, error) {
	var err error
	var repos apicontract.RepoPage
	if err = s.invoke("repo_list", apicontract.RepoListArgs{}, &repos); err != nil {
		return "", err
	}
	branch := ""
	for _, r := range repos.Repositories {
		if r.Repo == repo {
			branch = r.DefaultBranch
		}
	}
	if branch == "" {
		problem := flowError("repository_missing", nil)
		problem.Context = []string{"Repository: " + repo}
		return "", problem
	}
	ref := "refs/remotes/oberth/" + branch
	if _, err = s.git("fetch", "--", "oberth", "refs/heads/"+branch+":"+ref); err != nil {
		return "", err
	}
	sha, err := s.git("rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	head, err := s.git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	if head != sha {
		return "", flowError("release_head", nil)
	}
	return sha, s.releaseGreen(repo, sha)
}

func (s *flowSession) releaseGreen(repo, sha string) error {
	green, err := s.waitFor(apicontract.WaitArgs{Repo: repo, SHA: sha})
	if err != nil {
		return err
	}
	if green.Status != stepmodel.RunPassed {
		return flowError("green", nil)
	}
	if green.CommitSHA == nil || *green.CommitSHA != sha {
		return flowError("contract", nil)
	}
	return nil
}

func (s *flowSession) logStatus(logs apicontract.LogResult, id string, cursor *logCursor) (stepmodel.RunSummary, error) {
	var err error
	var run stepmodel.RunSummary
	if !s.options.json {
		if logs.Content != "" {
			cursor.openLine = !strings.HasSuffix(logs.Content, "\n")
		}
		if _, err = s.app.out.Write([]byte(logs.Content)); err != nil {
			return run, err
		}
	}
	return s.logRunInfo(id)
}

func passedPromotion(status apicontract.PromotionWaitResult) error {
	if status.Promotion.Status != model.PromotionPassed {
		problem := flowError("promotion", nil)
		problem.Context = []string{status.Promotion.Reason}
		if status.Promotion.Detail != "" {
			// The cause, such as the conflict a retest met, is read with the reason.
			problem.Context = append(problem.Context, status.Promotion.Detail)
		}
		return problem
	}
	if status.Promotion.ResultSHA == "" || status.Publication == nil {
		return flowError("contract", nil)
	}
	if p := status.Publication; p.Status != model.PublicationDelivered {
		problem := flowError("publication", nil)
		problem.Context = []string{"Publication: " + p.ID + " " + string(p.Status) + ": " + p.Reason}
		problem.Detail = p.Detail
		return problem
	}
	return nil
}

func (s *flowSession) commit(args []string) (string, error) {
	if !validFlowArgs("promote", args) {
		return "", usageProblems{"promote accepts at most one commit"}
	}
	ref := "HEAD"
	if len(args) == 1 {
		ref = args[0]
	}
	sha, err := s.git("rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", usageProblems{"Cannot resolve commit " + commandWord(ref) + "; choose a local commit."}
	}
	return sha, nil
}

func (s *flowSession) releaseReceipt(run string) error {
	after := int64(0)
	for {
		var result apicontract.EventResult
		if err := s.invoke("events_wait", apicontract.EventsWaitArgs{AfterID: &after, Kinds: []string{string(model.EventReleasePublished), string(model.EventPublicationFailed)}, RunIDs: []string{run}}, &result); err != nil {
			return err
		}
		if result.Gap {
			return flowError("cursor", nil)
		}
		for _, event := range result.Events {
			if event.Kind == string(model.EventPublicationFailed) {
				return flowError("release", nil)
			}
			if event.Kind == string(model.EventReleasePublished) {
				return s.fact("release.published: %d\nnext: oberth log %s", event.ID, run)
			}
		}
		after = result.NextAfterID
	}
}

type logCursor struct {
	offset    *int64
	selection string
	openLine  bool
}

func (s *flowSession) currentLogPage(request apicontract.RunLogsArgs, cursor *logCursor) (*apicontract.LogResult, error) {
	request.ByteOffset = cursor.offset
	var logs apicontract.LogResult
	if err := s.invoke("run_logs", request, &logs); err != nil {
		return nil, err
	}
	chosen := logContainerKey(logs)
	if chosen != cursor.selection {
		if err := s.endLogLine(cursor); err != nil {
			return nil, err
		}
		if chosen != "" {
			if err := s.fact("container: %s", logs.Containers[0].Container); err != nil {
				return nil, err
			}
		}
		cursor.selection = chosen
		if cursor.offset != nil {
			cursor.offset = nil
			return nil, nil
		}
	}
	return &logs, nil
}

func (s *flowSession) followStepLog(request apicontract.RunLogsArgs, step stepmodel.RunStep) (stepmodel.RunSummary, error) {
	var cursor logCursor
	for {
		logs, err := s.currentLogPage(request, &cursor)
		if err != nil {
			return stepmodel.RunSummary{}, err
		}
		if logs == nil {
			continue
		}
		run, err := s.logStatus(*logs, request.ID, &cursor)
		if err != nil {
			return run, err
		}
		if logs.Meta.HasMore && cursor.offset != nil && *cursor.offset == logs.Meta.NextByteOffset {
			return run, flowError("log_cursor", nil)
		}
		next := logs.Meta.NextByteOffset
		cursor.offset = &next
		if logs.Meta.HasMore {
			continue
		}
		if logOutputFinished(run, step.Path, logs.Meta) {
			return run, s.finishLogStep(*logs, &cursor, run, step.Path)
		}
		if err := s.app.config.WaitFor(s.ctx, time.Second); err != nil {
			return run, err
		}
	}
}

func (s *flowSession) endLogLine(cursor *logCursor) error {
	if !cursor.openLine {
		return nil
	}
	cursor.openLine = false
	_, err := s.app.out.Write([]byte("\n"))
	return err
}

func (s *flowSession) finishLogStep(logs apicontract.LogResult, cursor *logCursor, run stepmodel.RunSummary, path string) error {
	if err := s.endLogLine(cursor); err != nil {
		return err
	}
	for _, step := range run.Steps {
		if step.Path == path && step.Status == stepmodel.StepFailed {
			return s.fact("%s\n%s", step.DisplayReason, step.Detail)
		}
	}
	reason := logs.DisplayReason
	if reason == "" {
		reason = logs.Meta.Reason
	}
	if logs.Content == "" && reason != "" {
		return s.fact("%s", reason)
	}
	return nil
}
