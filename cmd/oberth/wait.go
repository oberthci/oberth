package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
)

// This command exists because every caller that needed to wait for a run wrote
// its own poller, and three of them were wrong in the same session: one gave
// up at the server's own 120 second read cap and reported a timeout for a run
// that was still queued, one treated "queued" as terminal and declared a green
// run before a single step had started, and one exited zero on a red run
// because it only checked whether the request succeeded. Waiting is one
// behaviour and it belongs in one place.

const (
	// defaultWaitTimeout is generous on purpose. A cold cache pulling a node
	// image and installing a dependency tree is minutes of honest work, and a
	// wait that expires during it teaches the reader to ignore the verdict.
	defaultWaitTimeout = 15 * time.Minute
	// waitSettleAttempts is how many consecutive lookup failures are absorbed
	// before giving up. A run submitted a moment ago is not yet listed, and a
	// server restart mid-wait should not be reported as a red run.
	waitSettleAttempts = 12
)

// waitPollInterval is how often the run is re-read. The server's own long-poll
// caps out well below the total wait, so the loop continues past that cap
// rather than mistaking it for an answer. It is a variable so the tests can
// exercise the loop without spending real minutes in it.
var waitPollInterval = 5 * time.Second

// errRunFailed makes `oberth wait` exit non-zero on a red run without printing
// a second error line: the verdict already said everything.
var errRunFailed = errors.New("")

func runWait(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	timeout := flags.Duration("timeout", defaultWaitTimeout, "give up after this long")
	quiet := flags.Bool("quiet", false, "print the verdict only, without progress")
	if err := flags.Parse(permuteFlagsFirst(arguments)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("%w: wait <sha|run-id> [--timeout 15m]", errUsage)
	}
	if *timeout <= 0 {
		return fmt.Errorf("%w: --timeout must be positive", errUsage)
	}
	api, err := remoteClient(ctx)
	if err != nil {
		return err
	}
	reportMode("server")
	run, err := waitForRun(ctx, api, flags.Arg(0), *timeout, progressWriter(output, *quiet))
	if err != nil {
		return err
	}
	return printVerdict(output, run)
}

func progressWriter(output io.Writer, quiet bool) io.Writer {
	if quiet {
		return io.Discard
	}
	return output
}

// waitForRun polls until the run reaches a terminal status, the deadline
// passes, or the context is cancelled.
func waitForRun(ctx context.Context, api *client.Client, selector string,
	timeout time.Duration, progress io.Writer) (remoteRun, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	lastStatus := ""
	misses := 0
	var lastErr error
	for {
		run, found, err := lookupRun(ctx, api, selector)
		switch {
		case err != nil:
			// A transient read failure is absorbed: a server restart during a
			// ten-minute wait must not be reported as a verdict.
			lastErr = err
			misses++
			if misses > waitSettleAttempts {
				return remoteRun{}, fmt.Errorf("reading the run: %w", err)
			}
		case !found:
			// The run is not listed yet. A push that has just been accepted
			// is exactly this case, and it is not an error.
			misses++
			if misses > waitSettleAttempts {
				return remoteRun{}, fmt.Errorf("no run for %q; nothing has been submitted for it", selector)
			}
		default:
			misses, lastErr = 0, nil
			if run.Status != lastStatus {
				lastStatus = run.Status
				fmt.Fprintf(progress, "%s  %s\n", shortRunID(run.ID), run.Status)
			}
			if terminalRunStatus(run.Status) {
				return run, nil
			}
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return remoteRun{}, fmt.Errorf("gave up after %s; the last read failed: %w", timeout, lastErr)
			}
			return remoteRun{}, fmt.Errorf("gave up after %s with the run still %s",
				timeout, statusOrUnknown(lastStatus))
		}
		select {
		case <-ctx.Done():
			return remoteRun{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func statusOrUnknown(status string) string {
	if status == "" {
		return "unlisted"
	}
	return status
}

// terminalRunStatus is the whole definition of "done", in one place.
//
// queued and running are NOT terminal. Treating queued as terminal is what
// produced a green verdict for a run that had not started.
func terminalRunStatus(status string) bool {
	switch model.RunStatus(status) {
	case model.RunPassed, model.RunFailed, model.RunInterrupted:
		return true
	default:
		return false
	}
}

// lookupRun finds the run named by a full ID, an abbreviation, or a commit.
//
// A commit selector takes the most recent run for that SHA, which is the run
// the push that just happened created.
func lookupRun(ctx context.Context, api *client.Client, selector string) (remoteRun, bool, error) {
	var runs []remoteRun
	if err := api.Get(ctx, "/api/runs", map[string]string{"limit": "100"}, &runs); err != nil {
		return remoteRun{}, false, err
	}
	// An exact run ID first: it is unambiguous and cannot collide with a SHA
	// prefix.
	for _, run := range runs {
		if run.ID == selector {
			return run, true, nil
		}
	}
	// Then a commit, newest first. /api/runs is ordered newest first, so the
	// first match is the latest run for that commit.
	lowered := strings.ToLower(selector)
	for _, run := range runs {
		if strings.HasPrefix(strings.ToLower(run.SHA), lowered) && len(lowered) >= 7 {
			return run, true, nil
		}
	}
	// Finally a run-ID abbreviation, which the push banner prints.
	if looksAbbreviated(selector) {
		var matched remoteRun
		hits := 0
		for _, run := range runs {
			if strings.HasPrefix(run.ID, lowered) {
				matched, hits = run, hits+1
			}
		}
		if hits > 1 {
			return remoteRun{}, false, fmt.Errorf("%q matches %d runs; use more characters", selector, hits)
		}
		if hits == 1 {
			return matched, true, nil
		}
	}
	return remoteRun{}, false, nil
}

// printVerdict is the one line a caller reads, and the exit status a script
// branches on.
func printVerdict(output io.Writer, run remoteRun) error {
	if model.RunStatus(run.Status) == model.RunPassed {
		_, err := fmt.Fprintf(output, "green  %s  %s  %s\n", run.ID, shortSHA(run.SHA), run.Ref)
		return err
	}
	where := ""
	if run.FailedBurn != "" || run.FailedStep != "" {
		where = fmt.Sprintf("  at %s/%s", run.FailedBurn, run.FailedStep)
	}
	if _, err := fmt.Fprintf(output, "%s  %s  %s  %s%s\n",
		run.Status, run.ID, shortSHA(run.SHA), run.Ref, where); err != nil {
		return err
	}
	if reason := strings.TrimSpace(run.Error); reason != "" {
		if _, err := fmt.Fprintf(output, "  %s\n", reason); err != nil {
			return err
		}
	}
	if run.FailedBurn != "" && run.FailedStep != "" {
		if _, err := fmt.Fprintf(output, "  oberth log %s --burn %s --step %s\n",
			run.ID, run.FailedBurn, run.FailedStep); err != nil {
			return err
		}
	}
	return errRunFailed
}

func shortRunID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
