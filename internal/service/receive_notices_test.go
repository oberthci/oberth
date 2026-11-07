package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func TestDescribePushResultCoversDurableOutcomes(t *testing.T) {
	t.Parallel()
	branch := gitcache.ReceiveEvent{
		Repo:   "acme-cli",
		Update: gitcache.RefUpdate{Ref: "refs/heads/testb", NewSHA: "1111111111111111111111111111111111111111"},
	}
	tag := gitcache.ReceiveEvent{
		Repo:   "acme-cli",
		Update: gitcache.RefUpdate{Ref: "refs/tags/v1.2.3", NewSHA: "2222222222222222222222222222222222222222"},
	}
	run := &model.Run{ID: "run-0123456789abcdef"}
	cases := []struct {
		name   string
		event  gitcache.ReceiveEvent
		result PushResult
		want   string
	}{
		{
			name: "branch run queued", event: branch,
			result: PushResult{Run: run},
			want:   "ref accepted · run run-01234567 queued (acme-cli/testb)",
		},
		{
			name: "queued supersession without workflow cleanup", event: branch,
			result: pushRunResult(model.Repository{}, model.EnqueueRunResult{Run: *run, SupersededCount: 1}),
			want:   "ref accepted · run run-01234567 queued (acme-cli/testb) · superseded 1 earlier run",
		},
		{
			name: "branch deletion", event: branch,
			result: PushResult{Deleted: true},
			want:   "branch testb deleted on the mirror · upstream deletion queued — no run",
		},
		{
			name: "replayed duplicate with supersession", event: branch,
			result: PushResult{Run: run, Duplicate: true, SupersededCount: 2},
			want:   "ref accepted · run run-01234567 queued (acme-cli/testb) · replay of an already-recorded push · superseded 2 earlier runs",
		},
		{
			name: "release admitted", event: tag,
			result: PushResult{Run: run, Admitted: true},
			want:   "release tag accepted · release run run-01234567 queued (acme-cli/v1.2.3)",
		},
		{
			name: "release rejected", event: tag,
			result: PushResult{Reason: "release tags are immutable"},
			want:   "tag v1.2.3: release tags are immutable",
		},
		{
			name: "recorded without run or reason", event: branch,
			result: PushResult{},
			want:   "branch testb recorded",
		},
	}
	for _, testCase := range cases {
		if got := describePushResult(testCase.event, testCase.result); got != testCase.want {
			t.Errorf("%s:\n got  %q\n want %q", testCase.name, got, testCase.want)
		}
	}
}

func TestWithNoticesFallsBackToPlainHandlerWithoutSink(t *testing.T) {
	t.Parallel()
	handler := &ReceiveHandler{Pushes: &PushIngestor{}}
	if handler.WithNotices(nil) != gitcache.ReceiveHandler(handler) {
		t.Fatal("nil notice sink must return the undecorated handler")
	}
}

func TestReceiveNoticesRetainOnlySuccessfulDurableRunIDs(t *testing.T) {
	result := PushResult{Run: &model.Run{ID: "full-durable-run-id"}}
	var failure error
	handler := &receiveNotices{notify: func(string, ...any) {}, dispatch: func(context.Context, gitcache.ReceiveEvent) (PushResult, error) { return result, failure }}
	event := gitcache.ReceiveEvent{Repo: "repo", Update: gitcache.RefUpdate{Ref: "refs/heads/main"}}
	for range 2 {
		if err := handler.HandleReceive(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	result.Run = &model.Run{ID: "not-accepted"}
	failure = errors.New("refused")
	if handler.HandleReceive(t.Context(), event) == nil {
		t.Fatal("refusal lost")
	}
	if got := handler.QueuedRunIDs(); !reflect.DeepEqual(got, []string{"full-durable-run-id"}) {
		t.Fatal(got)
	}
}
