//go:build linux && amd64

package trustedharness

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func TestHarnessProtectedTransportIntegration(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "native-fixture")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "./testdata/fixture.go")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native fixture: %v: %s", err, out)
	}
	driver, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"pass", "fail", "timeout", "changed guest"} {
		t.Run(scenario, func(t *testing.T) {
			guest := guestBundle(t, []string{"kernel", "initramfs"})
			guestHash := digest(guest)
			cases := []string{"first", "second"}
			if scenario == "fail" || scenario == "timeout" {
				cases[1] = scenario
			}
			manifest := []byte(fmt.Sprintf("version: 1\nprofile: kvm-suite-amd64-v1\nguest_sha256: %s\nartifacts: [program]\ncases: [%s]\n", guestHash, strings.Join(cases, ", ")))
			var bundle bytes.Buffer
			tw := tar.NewWriter(&bundle)
			for _, file := range []struct {
				name string
				body []byte
			}{{"manifest.yaml", manifest}, {DriverFile, driver}} {
				if err := tw.WriteHeader(&tar.Header{Name: file.name, Size: int64(len(file.body)), Mode: 0555, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
					t.Fatal(err)
				}
				if _, err := tw.Write(file.body); err != nil {
					t.Fatal(err)
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			contract := vmrunner.Contract{Version: 2, Profile: vmrunner.KVMSuiteAMD64Profile, SuiteSHA256: digest(bundle.Bytes()), GuestSHA256: guestHash, Artifacts: []string{"program"}}
			verified, err := vmrunner.VerifyKVMSuiteBundle(contract, bundle.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			inventory, count := verified.Inventory()
			now := time.Now().UTC()
			intent := vmrunner.KVMIntent{RunID: "fixture", RepoID: 1, Repository: "upstream/org/repo", CandidateSHA: strings.Repeat("a", 40), BaselineSHA: strings.Repeat("b", 40), Contract: contract, Recipe: vmrunner.KVMRecipe{SuiteSHA256: contract.SuiteSHA256, GuestSHA256: guestHash, HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("c", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"}, Namespace: "pipelines", WorkflowName: "fixture-trusted", WorkflowSpecDigest: strings.Repeat("d", 64), InventoryDigest: inventory, CaseCount: count, AdmittedAt: now, Deadline: now.Add(5 * time.Second)}
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "program"), []byte("untrusted candidate bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			var qemu *exec.Cmd
			var argv []string
			deps := harnessDependencies{fetch: func(_ context.Context, url, _ string, _ int64) ([]byte, error) {
				if strings.HasSuffix(url, "suite") {
					return bundle.Bytes(), nil
				}
				if scenario == "changed guest" {
					return []byte("wrong bytes"), nil
				}
				return guest, nil
			}, qemu: func(args ...string) *exec.Cmd {
				argv = append([]string(nil), args...)
				qemu = exec.Command(binary, append([]string{"qemu"}, args...)...)
				return qemu
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// Keep socket paths short while respecting the runner's writable TMPDIR.
			private, err := os.MkdirTemp("", "kh-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(private)
			report, err := run(ctx, intent, root, private, deps)
			if scenario == "pass" {
				if err != nil {
					t.Fatal(err)
				}
				if report.InventoryDigest != inventory || report.CaseCount != 2 || report.ArtifactSetDigest == "" {
					t.Fatalf("wrong actual completed inventory: %#v", report)
				}
			} else if err == nil {
				t.Fatal("negative execution produced pass")
			}
			if qemu != nil && qemu.ProcessState == nil {
				t.Fatal("QEMU not joined")
			}
			if qemu != nil {
				joined := strings.Join(argv, " ")
				for _, must := range []string{"-sandbox on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny", "-accel kvm", "-nic none", "-nodefaults", "-no-shutdown", "-fw_cfg name=opt/oberth/artifacts,file="} {
					if !strings.Contains(joined, must) {
						t.Fatalf("guest boundary missing %s", must)
					}
				}
				for _, bad := range []string{"-virtfs", "-fsdev", "hostfwd", "suite-driver"} {
					if strings.Contains(joined, bad) {
						t.Fatalf("guest reached trusted boundary: %s", bad)
					}
				}
			}
		})
	}
	t.Run("missing QMP reaches readiness deadline", func(t *testing.T) {
		private, err := os.MkdirTemp("", "kh-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(private)
		guest := filepath.Join(private, "guest.sock")
		listener, err := net.Listen("unix", guest)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		served := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				err = json.NewEncoder(conn).Encode(map[string]string{"cases": "invented guest PASS"})
				conn.Close()
			}
			served <- err
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "--case", "first", "--qmp", filepath.Join(private, "missing.sock"), "--guest-control", guest)
		out, err := cmd.CombinedOutput()
		listener.Close()
		if serveErr := <-served; serveErr != nil {
			t.Fatalf("guest response: %v", serveErr)
		}
		var exit *exec.ExitError
		if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Fatalf("missing QMP must reach the fixture readiness deadline: %v: %s", err, out)
		}
	})

}
