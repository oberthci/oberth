package gitcache_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceivePackLimitRefusesBeforeRefsMove(t *testing.T) {
	for _, sideband := range []bool{false, true} {
		t.Run(fmt.Sprintf("sideband=%t", sideband), func(t *testing.T) { checkReceivePackLimit(t, sideband) })
	}
}

func checkReceivePackLimit(t *testing.T, sideband bool) {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.MaxInputBytes = 100
	var logs []string
	cfg.Log = func(_, detail string) { logs = append(logs, detail) }
	c := thirdInit(t, cfg)
	r := gateReceive(f)
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/oversized", NewSHA: f.Commit})
	if sideband {
		enableReceiveSideband(t, &r)
	}
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("oversized pack delivered")
		return nil, nil
	}
	var feedback, protocol bytes.Buffer
	r.Diagnostics, r.Output = &feedback, &protocol
	if err := c.Receive(t.Context(), r); err == nil {
		t.Fatal("oversized pack accepted")
	}
	if !strings.Contains(feedback.String(), "pack exceeds 100 bytes") || strings.Contains(feedback.String()+protocol.String(), "fatal:") {
		t.Fatal("limit refusal lost or leaked native detail", feedback.String())
	}
	if !strings.Contains(strings.Join(logs, "\n"), "pack exceeds maximum allowed size") {
		t.Fatal("native pack limit detail not logged", logs)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "oversized") {
		t.Fatal("oversized pack moved refs", refs)
	}
	if _, err := c.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
		t.Fatal("oversized pack left a receive", err)
	}
}

func enableReceiveSideband(t *testing.T, r *gitcache.ReceiveRequest) {
	t.Helper()
	data, err := io.ReadAll(r.Input)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Input.Close()
	n, err := strconv.ParseUint(string(data[:4]), 16, 16)
	if err != nil {
		t.Fatal(err)
	}
	line := string(data[4:n-1]) + " side-band-64k\n"
	r.Input = io.NopCloser(io.MultiReader(strings.NewReader(fmt.Sprintf("%04x%s", len(line)+4, line)), bytes.NewReader(data[n:])))
}

func TestReceiveReferenceLimitNamesBound(t *testing.T) {
	f := gittest.New(t)
	c := thirdInit(t, reviewConfig(t, f))
	updates := make([]gitcache.RefUpdate, gitcache.MaxUpdates+1)
	for i := range updates {
		updates[i] = gitcache.RefUpdate{Ref: fmt.Sprintf("refs/heads/topic-%d", i), NewSHA: f.Commit}
	}
	r := fourthAgentPush(t, f, updates, nil)
	var feedback bytes.Buffer
	r.Diagnostics = &feedback
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("over-limit refs delivered")
		return nil, nil
	}
	_ = c.Receive(t.Context(), r)
	if !strings.Contains(feedback.String(), "push has more than 256 refs") {
		t.Fatal("refusal omitted reference bound", feedback.String())
	}
}

func TestReceivePackDefaultIsOneGiB(t *testing.T) {
	f := gittest.New(t)
	_ = thirdInit(t, reviewConfig(t, f))
	data, err := os.ReadFile(filepath.Join(f.Root, "cache", "oberth.gitconfig"))
	if err != nil || !strings.Contains(string(data), "maxInputSize=1073741824") {
		t.Fatal("default incoming pack limit missing", string(data), err)
	}
}

func TestReceiveNativeDiagnosticsStayInServerLog(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	path := filepath.Join(f.Root, "native-failure")
	body := "#!/bin/sh\ncase \" $* \" in *' receive-pack '*) printf 'fatal: private receive path\\n' >&2; exit 128;; esac\nexec " + shellQuote(f.Binary) + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = path
	var logs []string
	cfg.Log = func(_, detail string) { logs = append(logs, detail) }
	c := thirdInit(t, cfg)
	r := gateReceive(f)
	r.Input = io.NopCloser(strings.NewReader("0000"))
	var feedback bytes.Buffer
	r.Diagnostics = &feedback
	err := c.Receive(t.Context(), r)
	if err == nil || strings.Contains(feedback.String()+err.Error(), "private receive path") {
		t.Fatal("native failure escaped to pusher", err, feedback.String())
	}
	if !strings.Contains(strings.Join(logs, "\n"), "private receive path") {
		t.Fatal("native failure missing from server log", logs)
	}
}
