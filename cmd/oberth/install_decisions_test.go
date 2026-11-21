package main

import (
	"context"
	"io"
	"os/exec"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestInstallationOutputModesBeforeDispatch(t *testing.T) {
	for _, command := range []string{"install", "upgrade", "backup"} {
		for _, format := range []string{"", "text", "json", "yaml"} {
			for _, dry := range []bool{false, true} {
				t.Run(command+"/"+format+map[bool]string{true: "/dry-run"}[dry], func(t *testing.T) {
					installationOutputCase(t, command, format, dry)
				})
			}
		}
	}
}

func installationOutputCase(t *testing.T, command, format string, dry bool) {
	t.Helper()
	app, out, diagnostic := fixture(t, fake.Bindings())
	calls := 0
	app.install = func(context.Context, installationRequest, io.Reader, io.Writer) error {
		calls++
		return nil
	}
	args := []string{command, "--output=" + format}
	if dry {
		args = append(args, "--dry-run")
	}
	wantCode, wantCalls := 0, 1
	if format == "yaml" || format == "json" && command != "backup" && !dry {
		wantCode, wantCalls = 2, 0
	}
	if code := app.run(t.Context(), args); code != wantCode || calls != wantCalls || out.Len() != 0 {
		t.Fatal("output mode reached the wrong operation", code, calls, out.String(), diagnostic.String())
	}
}

func TestSecretStepRejectsCommandBeforeSeparator(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.secretExec = func(context.Context, installationRequest, io.Reader, io.Writer) error {
		t.Fatal("invalid command reached secret delivery")
		return nil
	}
	code := app.run(t.Context(), []string{"secretstore", "exec", "misplaced", "--", "true"})
	want := "Command arguments are invalid.\nPlace step options before -- and the command after it.\nUse oberth secretstore exec -- <command> [args].\nRun oberth help for command names and arguments.\n"
	if code != 2 || out.Len() != 0 || diagnostic.String() != want {
		t.Fatal(code, out.String(), diagnostic.String())
	}
}

func TestStoreOperatorRequiresConfiguration(t *testing.T) {
	for _, command := range []string{"setup", "check"} {
		t.Run(command, func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			code := app.run(t.Context(), []string{"secretstore", command})
			want := "Secret-store configuration is missing.\nPass --config with the installation configuration and its one store audience.\n"
			if code != 1 || out.Len() != 0 || diagnostic.String() != want {
				t.Fatal(code, out.String(), diagnostic.String())
			}
		})
	}
}

func TestSecretStepDistinguishesDeliveryRefusalFromChildExit(t *testing.T) {
	child := exec.Command("sh", "-c", "exit 23").Run()
	if child == nil {
		t.Fatal("child did not fail")
	}
	refusal := publicerr.New(publicerr.Unavailable, "Secret delivery failed.", "Check the secret store, then retry.", child)
	for _, wrapped := range []bool{false, true} {
		t.Run(map[bool]string{false: "child", true: "delivery"}[wrapped], func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			app.secretExec = func(context.Context, installationRequest, io.Reader, io.Writer) error {
				if wrapped {
					return refusal
				}
				return child
			}
			wantCode, wantMessage := 23, ""
			if wrapped {
				wantCode, wantMessage = 1, "Secret delivery failed.\nCheck the secret store, then retry.\n"
			}
			if code := app.run(t.Context(), []string{"secretstore", "exec", "--", "tool"}); code != wantCode || out.Len() != 0 || diagnostic.String() != wantMessage {
				t.Fatal(code, out.String(), diagnostic.String())
			}
		})
	}
}
