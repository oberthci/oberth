package sourceplugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"google.golang.org/grpc/codes"
)

func TestTokenEnvironmentUsesOnlyNativeMount(t *testing.T) {
	t.Setenv("KUBECONFIG", "/unused/config")
	if Environment().TokenFile != common.ServiceAccountTokenMountPath+"/token" {
		t.Fatal("default does not match the native token mount")
	}
	for _, path := range []string{"", "relative", "/projected/../token"} {
		cfg := config(t)
		cfg.TokenFile = path
		if client, err := New(cfg); err == nil {
			client.Close()
			t.Fatal("invalid token path accepted")
		}
	}
}

func TestTokenFileRefusalsNamePathWithoutDisclosingContents(t *testing.T) {
	for _, kind := range []string{"missing", "missing directory", "directory", "empty", "invalid", "absolute escape", "relative escape"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			switch kind {
			case "missing directory":
				path = filepath.Join(filepath.Dir(path), "absent", "token")
			case "directory":
				identityOK(t, os.Mkdir(path, 0700))
			case "empty":
				identityOK(t, os.WriteFile(path, nil, 0600))
			case "invalid":
				identityOK(t, os.WriteFile(path, []byte("private:contents"), 0600))
			case "absolute escape", "relative escape":
				target := identity(t)
				if kind == "relative escape" {
					var err error
					target, err = filepath.Rel(filepath.Dir(path), target)
					identityOK(t, err)
				}
				identityOK(t, os.Symlink(target, path))
			}
			_, err := (&Client{tokenFile: path}).bearer()
			if err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "private:contents") {
				t.Fatal("identity refusal must name only the configured path")
			}
			if loadCode(t.Context(), err) != codes.FailedPrecondition {
				t.Fatal("identity failure lost its RPC status")
			}
		})
	}
}

func TestProjectedTokenSymlinkRotation(t *testing.T) {
	dir := t.TempDir()
	for _, version := range []string{"first", "second"} {
		identityOK(t, os.Mkdir(filepath.Join(dir, version), 0700))
		identityOK(t, os.WriteFile(filepath.Join(dir, version, "token"), []byte(version), 0600))
	}
	identityOK(t, os.Symlink("first", filepath.Join(dir, "..data")))
	identityOK(t, os.Symlink("..data/token", filepath.Join(dir, "token")))
	client := &Client{tokenFile: filepath.Join(dir, "token")}
	for _, version := range []string{"first", "second"} {
		identityOK(t, os.Symlink(version, filepath.Join(dir, "..next")))
		identityOK(t, os.Rename(filepath.Join(dir, "..next"), filepath.Join(dir, "..data")))
		got, err := client.bearer()
		identityOK(t, err)
		if got != version {
			t.Fatal("projected token rotation was not reread")
		}
	}
}

func identityOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
