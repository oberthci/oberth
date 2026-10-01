package service

import (
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
			name: "branch deletion", event: branch,
			result: PushResult{Deleted: true},
			want:   "branch testb deletion recorded — no run",
		},
		{
			name: "replayed duplicate with supersession", event: branch,
			result: PushResult{Run: run, Duplicate: true, Cancellations: []model.RunCancellation{{}, {}}},
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
