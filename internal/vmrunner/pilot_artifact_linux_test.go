//go:build linux

package vmrunner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPilotArtifactUsesCapturedBytesAndRejectsChangedInputs(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, BeaconArtifact)
	if err := os.WriteFile(path, []byte("first immutable artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, err := CaptureBeaconArtifact(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	plan := testPilotPlan()
	plan.ArtifactDigest, plan.ArtifactBytes = captured.Digest(), captured.Size()
	if err := os.WriteFile(path, []byte("replacement build bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	reader, err := captured.Open(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != "first immutable artifact" {
		t.Fatalf("execution reopened mutable path: %q, %v", body, err)
	}
	changed, err := CaptureBeaconArtifact(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changed.Open(plan); err == nil {
		t.Fatal("changed bytes reused old input seal")
	}
	plan.ArtifactBytes++
	if _, err := captured.Open(plan); err == nil {
		t.Fatal("wrong sealed size accepted")
	}
}

func TestPilotArtifactRejectsSymlinksSpecialFilesAndOversize(t *testing.T) {
	for _, kind := range []string{"leaf-symlink", "parent-symlink", "hard-link", "fifo", "directory", "oversize", "empty"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, BeaconArtifact)
			switch kind {
			case "leaf-symlink":
				if err := os.WriteFile(filepath.Join(root, "payload"), []byte("candidate"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../payload", path); err != nil {
					t.Fatal(err)
				}
			case "parent-symlink":
				if err := os.Rename(bin, filepath.Join(root, "real-bin")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("real-bin", bin); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("candidate"), 0600); err != nil {
					t.Fatal(err)
				}
			case "hard-link":
				payload := filepath.Join(root, "payload")
				if err := os.WriteFile(payload, []byte("candidate"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(payload, path); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				file, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "oversize" {
					err = file.Truncate(MaxBeaconArtifactBytes + 1)
				}
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatal(errors.Join(err, closeErr))
				}
			}
			if _, err := CaptureBeaconArtifact(context.Background(), root); err == nil {
				t.Fatalf("accepted %s artifact", kind)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CaptureBeaconArtifact(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}
