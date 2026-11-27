package service_test

import (
	"context"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

type testGit struct {
	Resolve  func(context.Context, string, string) (gitport.Ref, error)
	Refs     []gitport.Ref
	RefError error
	Release  func(context.Context, stateport.Repository, gitport.Ref) error
}

func (f *testGit) ResolveRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	return f.Resolve(ctx, repo, ref)
}
func (f *testGit) ListRefs(context.Context, string) ([]gitport.Ref, error) {
	return append([]gitport.Ref{}, f.Refs...), f.RefError
}
func (f *testGit) ReleaseTag(ctx context.Context, r stateport.Repository, ref gitport.Ref) (gitport.Ref, error) {
	return gitport.Ref{Name: "refs/heads/main", ObjectSHA: ref.CommitSHA, CommitSHA: ref.CommitSHA}, f.Release(ctx, r, ref)
}

type testExec struct {
	CancelFunc  func(context.Context, stateport.Run, bool) error
	InspectFunc func(context.Context, service.CheckRequest) (apicontract.RunCheckResult, error)
	LockValues  []service.Lock
}

func (f *testExec) Cancel(ctx context.Context, r stateport.Run, terminate bool) error {
	return f.CancelFunc(ctx, r, terminate)
}
func (f *testExec) Inspect(ctx context.Context, q service.CheckRequest) (apicontract.RunCheckResult, error) {
	return f.InspectFunc(ctx, q)
}
func (f *testExec) Locks(context.Context) ([]service.Lock, error) {
	return append([]service.Lock{}, f.LockValues...), nil
}

type testStarter struct {
	Start func(context.Context, service.StartRequest) (apicontract.Receipt, error)
}

func (f *testStarter) StartRun(ctx context.Context, q service.StartRequest) (apicontract.Receipt, error) {
	return f.Start(ctx, q)
}

type testLogs struct {
	ReadFunc  func(context.Context, stateport.Run, apicontract.RunLogsArgs) (apicontract.LogResult, error)
	PurgeFunc func(context.Context, stateport.Run) error
}

func (f *testLogs) Read(ctx context.Context, r stateport.Run, q apicontract.RunLogsArgs) (apicontract.LogResult, error) {
	return f.ReadFunc(ctx, r, q)
}
func (f *testLogs) Purge(ctx context.Context, r stateport.Run) error { return f.PurgeFunc(ctx, r) }

func (*testLogs) Usage(_ context.Context, r stateport.Run) (logcontract.Usage, error) {
	return logcontract.Usage{PhysicalRunID: r.ID}, nil
}

type testSource struct {
	File    func(context.Context, string, string) (gitport.Ref, gitport.File, error)
	Default func(context.Context, string) (string, error)
}

func (f *testSource) DefaultBranch(ctx context.Context, repo string) (string, error) {
	return f.Default(ctx, repo)
}

func (f *testSource) DefaultFile(ctx context.Context, repo, path string) (gitport.Ref, gitport.File, error) {
	return f.File(ctx, repo, path)
}
