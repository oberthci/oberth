package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestBinaryBuildIdentity(t *testing.T) {
	for _, test := range []struct{ name, flags, want string }{
		{"development", "", "oberth (devel) commit=unknown date=unknown\n"},
		{"release recipe", "-buildid= -X main.version=v9.8.7 -X main.commit=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "oberth v9.8.7 commit=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa date=unknown\n"},
		{"dated release", "-buildid= -X main.version=v1.2.3 -X main.commit=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb -X main.date=2026-11-17T12:34:56Z", "oberth v1.2.3 commit=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb date=2026-11-17T12:34:56Z\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			binary := buildIdentityBinary(t, test.flags)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "version")
			command.WaitDelay = 5 * time.Second
			out, err := command.CombinedOutput()
			if err != nil || string(out) != test.want {
				t.Fatalf("compiled binary identity: %v; got %q, want %q", err, out, test.want)
			}
		})
	}
}

func buildIdentityBinary(t *testing.T, flags string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "oberth")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-p=1", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags", flags, "-o", binary, ".")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	command.WaitDelay = 5 * time.Second
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build identity binary: %v: %s", err, out)
	}
	return binary
}
