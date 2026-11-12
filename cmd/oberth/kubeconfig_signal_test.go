package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func init() {
	if filepath.Base(os.Args[0]) == "oberth-signal-fixture" {
		main()
		os.Exit(0)
	}
}

func TestCLIPrivateKubeconfigSignalsAndCrashRecovery(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			cli := filepath.Join(bin, "oberth-signal-fixture")
			if err := os.Symlink(os.Args[0], cli); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(root, "record")
			script := "#!/bin/sh\nprintf %s \"$KUBECONFIG\" > '" + strings.ReplaceAll(record, "'", "'\\''") + "'\nexec /bin/sleep 30\n"
			if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(root, "operator-config")
			before := []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: https://unused.example.test\nusers:\n- name: fixture\n  user:\n    token: dummy-fixture-credential\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\n    user: fixture\ncurrent-context: fixture\n")
			if err := os.WriteFile(source, before, 0600); err != nil {
				t.Fatal(err)
			}
			env := append(os.Environ(), "HOME="+root, "XDG_CACHE_HOME="+filepath.Join(root, "cache"), "KUBECONFIG="+source, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd := exec.Command(cli, "client", "kubectl", "get", "pods")
			cmd.Env = env
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			outputPath := filepath.Join(root, "process-output")
			output, openErr := os.OpenFile(outputPath, os.O_CREATE|os.O_RDWR, 0600)
			if openErr != nil {
				t.Fatal(openErr)
			}
			defer output.Close()
			diagnostic := func() string { b, _ := os.ReadFile(outputPath); return string(b) }
			cmd.Stdout = output
			cmd.Stderr = output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
			deadline := time.Now().Add(10 * time.Second)
			private := ""
			for time.Now().Before(deadline) {
				if data, err := os.ReadFile(record); err == nil && len(data) > 0 {
					private = string(data)
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if private == "" {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatal("child did not start", diagnostic())
			}
			for path, mode := range map[string]os.FileMode{private: 0600, filepath.Dir(private): 0700} {
				st, err := os.Stat(path)
				if err != nil || st.Mode().Perm() != mode {
					t.Fatal("private permissions", path, err)
				}
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			restart := func() {
				t.Helper()
				next := exec.Command(cli, "version")
				next.Env = env
				if data, err := next.CombinedOutput(); err != nil {
					t.Fatal("next startup failed", err, string(data))
				}
			}
			if sig == syscall.SIGKILL {
				restart()
				if _, err := os.Stat(private); err != nil {
					t.Fatal("surviving child lost its lease", err)
				}
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				deadline = time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					restart()
					if _, err := os.Stat(private); os.IsNotExist(err) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if _, err := os.Stat(private); !os.IsNotExist(err) {
				t.Fatal("credential copy survived cleanup/recovery", sig, err, diagnostic())
			}
			after, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatal("source changed", err)
			}
			st, err := os.Stat(source)
			if err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("source mode changed", err)
			}
			if _, err := os.Stat(source + ".lock"); !os.IsNotExist(err) {
				t.Fatal("source lock created", err)
			}
			t.Log(fmt.Sprint(sig), " removed copy, preserved source")
		})
	}
}
