package gitcache

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const shadowQueueSize = 16

// ShadowStats are process-local; errors and saturation are not mismatches.
type ShadowStats struct {
	Runs       uint64 `json:"shadow_runs"`
	Mismatches uint64 `json:"shadow_mismatches"`
	Errors     uint64 `json:"shadow_errors"`
	Skipped    uint64 `json:"shadow_skipped"`
}

type shadowJob struct {
	repo, path, sha string
	barrier         chan struct{}
}
type shadowComparison struct {
	SHA        string `json:"sha"`
	CLIArchive string `json:"cli_archive_sha256"`
	GoArchive  string `json:"go_archive_sha256"`
	CLITree    string `json:"cli_tree"`
	GoTree     string `json:"go_tree"`
}

type shadowState struct {
	runs, mismatches, failures, skipped atomic.Uint64
	ctx                                 context.Context
	cancel                              context.CancelFunc
	done                                chan struct{}
	jobs                                chan shadowJob
	command                             []string
	cache                               *Cache
}

func newShadowState(c *Cache, parent context.Context, command []string) *shadowState {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	s := &shadowState{ctx: ctx, cancel: cancel, done: make(chan struct{}), jobs: make(chan shadowJob, shadowQueueSize), cache: c}
	s.command = append([]string(nil), command...)
	if len(s.command) == 0 {
		if executable, err := os.Executable(); err == nil {
			s.command = []string{executable, "internal-gitcache-shadow"}
		}
	}
	go s.work()
	return s
}

func (c *Cache) queueShadow(path, sha string) {
	s := c.shadow
	if s == nil {
		return
	}
	if ValidateSHA(sha) != nil || s.ctx.Err() != nil {
		s.skipped.Add(1)
		return
	}
	repo, err := filepath.Rel(c.root, path)
	if err != nil || !filepath.IsLocal(repo) {
		s.skipped.Add(1)
		return
	}
	job := shadowJob{repo: strings.TrimSuffix(filepath.ToSlash(repo), ".git"), path: path, sha: sha}
	select {
	case s.jobs <- job:
	default:
		s.skipped.Add(1)
	}
}

// CloseShadow cancels and joins the bounded worker and its process group.
func (c *Cache) CloseShadow() {
	if c.shadow != nil {
		c.shadow.cancel()
		<-c.shadow.done
	}
}

// WaitShadow waits for observations submitted before this call. Delivery never
// calls this: it is useful when collecting a completed soak/diagnostic sample.
func (c *Cache) WaitShadow(ctx context.Context) error {
	if c.shadow == nil {
		return nil
	}
	done := make(chan struct{})
	select {
	case c.shadow.jobs <- shadowJob{barrier: done}:
	case <-c.shadow.done:
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-c.shadow.done:
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Cache) ShadowStats() ShadowStats {
	if c.shadow == nil {
		return ShadowStats{}
	}
	s := c.shadow
	stats := ShadowStats{Mismatches: s.mismatches.Load(), Errors: s.failures.Load(), Skipped: s.skipped.Load()}
	stats.Runs = s.runs.Load()
	return stats
}

func (c *Cache) WriteShadowMetrics(w io.Writer) {
	_, _ = fmt.Fprintf(w, "# HELP oberth_gitcache_shadow_mismatch_total CLI/go-git archive or tree disagreements.\n# TYPE oberth_gitcache_shadow_mismatch_total counter\noberth_gitcache_shadow_mismatch_total %d\n", c.ShadowStats().Mismatches)
}

func (s *shadowState) work() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.jobs:
			if s.ctx.Err() != nil {
				return
			}
			if job.barrier != nil {
				close(job.barrier)
				continue
			}
			s.runs.Add(1)
			ctx, cancel := context.WithTimeout(s.ctx, s.cache.timeout)
			comparison, err := s.compare(ctx, job)
			cancel()
			if err != nil {
				s.failures.Add(1)
				// Never print helper errors/stderr: these may contain source or
				// repository config. Only a fixed failure class is safe.
				reason := "helper_failed"
				if errors.Is(err, context.DeadlineExceeded) {
					reason = "timeout"
				}
				s.cache.logger.Printf("level=warning event=gitcache_shadow_error repo=%q sha=%q reason=%s", job.repo, job.sha, reason)
				continue
			}
			if comparison.CLIArchive != comparison.GoArchive || comparison.CLITree != comparison.GoTree {
				s.mismatches.Add(1)
				body, _ := json.Marshal(struct {
					Level string `json:"level"`
					Event string `json:"event"`
					Repo  string `json:"repo"`
					shadowComparison
				}{"warning", "gitcache_shadow_mismatch", job.repo, comparison})
				s.cache.logger.Printf("%s", body)
			}
		}
	}
}

func (s *shadowState) compare(ctx context.Context, job shadowJob) (shadowComparison, error) {
	var result shadowComparison
	if len(s.command) == 0 {
		return result, errors.New("shadow executable unavailable")
	}
	gitBinary, err := exec.LookPath(s.cache.gitBinary)
	if err != nil {
		return result, err
	}
	if !filepath.IsAbs(gitBinary) {
		gitBinary, err = filepath.Abs(gitBinary)
		if err != nil {
			return result, err
		}
	}
	args := append(append([]string(nil), s.command[1:]...), job.path, job.sha, gitBinary, s.cache.globalConfig)
	// Trusted executable, fixed internal arguments; no shell or credentials.
	cmd := exec.Command(s.command[0], args...) //nolint:gosec,noctx
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "GOMEMLIMIT=128MiB", "GOMAXPROCS=1", "GORACE=atexit_sleep_ms=0"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output shadowOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return result, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return result, err
		}
	case <-ctx.Done():
		// Let the helper cancel and reap its Git child before killing the
		// whole group. A stuck decoder still has a hard one-second grace.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(time.Second)
		select {
		case <-done:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
			<-done
		}
		return result, ctx.Err()
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		return result, err
	}
	if result.SHA != job.sha || ValidateSHA(result.CLITree) != nil || ValidateSHA(result.GoTree) != nil || !validShadowDigest(result.CLIArchive) || !validShadowDigest(result.GoArchive) {
		return result, errors.New("invalid shadow comparison")
	}
	return result, nil
}

type shadowOutput struct{ bytes.Buffer }

func (w *shadowOutput) Write(body []byte) (int, error) {
	if w.Len()+len(body) > 4096 {
		return 0, errors.New("oversized shadow output")
	}
	return w.Buffer.Write(body)
}

// Git's leading global PAX comment pins the commit actually served, even when
// the caller supplied a moving branch/tag. Never resolve that ref later.
type archivePrefix struct{ body []byte }

func (p *archivePrefix) Write(body []byte) (int, error) {
	n := len(body)
	if len(p.body) < 4096 {
		p.body = append(p.body, body[:min(len(body), 4096-len(p.body))]...)
	}
	return n, nil
}

func (p *archivePrefix) commit(ref string) string {
	if header, err := tar.NewReader(bytes.NewReader(p.body)).Next(); err == nil && header.Typeflag == tar.TypeXGlobalHeader {
		if sha := header.PAXRecords["comment"]; ValidateSHA(sha) == nil {
			return sha
		}
	}
	if ValidateSHA(ref) == nil {
		return ref
	}
	return ""
}
