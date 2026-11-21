package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

type killRestore struct{ Input, Identity, Destination string }
type plaintextBarrier struct {
	context.Context
	parent string
}

func (c plaintextBarrier) Err() error {
	stages, _ := filepath.Glob(filepath.Join(c.parent, ".oberth-state-*", "authenticated.tar"))
	for _, path := range stages {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			fmt.Println("STAGE " + filepath.Dir(path))
			select {}
		}
	}
	return c.Context.Err()
}

func TestRecoveryStageWorker(t *testing.T) {
	encoded := os.Getenv("OBERTH_C7_STAGE_WORKER")
	if encoded == "" {
		t.Skip("subprocess fixture")
	}
	var input killRestore
	must(t, json.Unmarshal([]byte(encoded), &input))
	c := config{mode: "restore", input: input.Input, identity: input.Identity, output: input.Destination, options: stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: store.Schema}}
	ctx := plaintextBarrier{Context: t.Context(), parent: filepath.Dir(c.output)}
	must(t, decrypt(ctx, c, io.Discard))
	t.Fatal("restore did not stop at its plaintext barrier")
}

func TestKilledRecoveryStageIsSweptOnOpenAndRetry(t *testing.T) {
	for _, mode := range []string{"open", "retry"} {
		t.Run(mode, func(t *testing.T) {
			c := cryptoFixture(t)
			must(t, encrypt(t.Context(), c, io.Discard))
			c = recoveryConfig(c)
			media := filepath.Join(filepath.Dir(c.input), "media")
			must(t, os.Mkdir(media, 0700))
			relocated := filepath.Join(media, "backup.age")
			must(t, os.Rename(c.input, relocated))
			c.input = relocated
			destination := filepath.Join(filepath.Dir(c.output), "database")
			must(t, os.Mkdir(destination, 0700))
			c.output = filepath.Join(destination, "restored.sqlite")
			stage := killAtPlaintext(t, c)
			if filepath.Dir(stage) != destination {
				t.Fatal("plaintext staged beside ciphertext", stage)
			}
			assertNoStage(t, media)
			if mode == "open" {
				body, err := os.ReadFile(c.database)
				must(t, err)
				live := filepath.Join(destination, "live.sqlite")
				must(t, os.WriteFile(live, body, 0600))
				s, err := store.Open(t.Context(), live, store.DefaultOptions(func() time.Time { return time.Time{} }))
				must(t, err)
				must(t, s.Close())
			} else {
				must(t, decrypt(t.Context(), c, io.Discard))
			}
			if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("killed restore plaintext was not swept", err)
			}
		})
	}
}

func killAtPlaintext(t *testing.T, c config) string {
	t.Helper()
	executable, err := os.Executable()
	must(t, err)
	encoded, err := json.Marshal(killRestore{c.input, c.identity, c.output})
	must(t, err)
	child := exec.CommandContext(t.Context(), executable, "-test.run=^TestRecoveryStageWorker$")
	child.Env = append(os.Environ(), "OBERTH_C7_STAGE_WORKER="+string(encoded))
	output, err := child.StdoutPipe()
	must(t, err)
	must(t, child.Start())
	t.Cleanup(func() {
		if child.ProcessState == nil {
			must(t, child.Process.Kill())
			_ = child.Wait()
		}
	})
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "STAGE ") {
		t.Fatal("restore did not reach plaintext barrier", scanner.Err())
	}
	stage := strings.TrimPrefix(scanner.Text(), "STAGE ")
	must(t, child.Process.Kill())
	if err := child.Wait(); err == nil {
		t.Fatal("restore worker was not killed")
	}
	return stage
}
