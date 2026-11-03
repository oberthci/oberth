//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFUSEGuestDoesNotRequireBPFKernel(t *testing.T) {
	mounts, err := guestMounts("fuse")
	if err != nil {
		t.Fatal(err)
	}
	foundDevices := false
	for _, mount := range mounts {
		if mount[2] == "devtmpfs" {
			foundDevices = true
		}
		if mount[2] == "tracefs" || mount[2] == "securityfs" || mount[2] == "cgroup2" {
			t.Fatalf("FUSE profile unexpectedly requires %s", mount[2])
		}
	}
	if !foundDevices {
		t.Fatal("FUSE profile missing guest device filesystem")
	}
	if _, err := guestMounts("unknown"); err == nil {
		t.Fatal("unknown guest profile admitted")
	}
}

func TestFUSEGuestFailsWithoutRealDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fuse")
	if err := requireFuse(path); err == nil || !strings.Contains(err.Error(), "required guest FUSE device") {
		t.Fatalf("missing FUSE: %v", err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := requireFuse(path); err == nil {
		t.Fatal("regular file substituted for FUSE device")
	}
}
