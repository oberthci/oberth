package client

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestRefusedInvitePreservesDeletedAndRecreatedHandoff(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "invite.json")
	var original, replacement os.FileInfo
	var pinned bool
	h := &fake.Handler[apicontract.UplinkInviteArgs, model.Invitation]{Run: func(context.Context) (model.Invitation, error) {
		var err error
		original, err = os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
		pinned = handoffDescriptorOpen(t, original)
		replacement = recreateDeletedHandoff(t, path, original)
		return model.Invitation{}, publicerr.New(publicerr.InvalidInput, "Invitation refused.", "Correct the invitation and retry.", nil)
	}}
	b := fake.Bindings()
	b.UplinkInvite.Handler = h
	c := testClient(t, handlerTransport(server(t, b)), nil)
	defer c.Close()
	_, err := c.Invite(t.Context(), apicontract.UplinkInviteArgs{Identity: "agent", ServerURL: "https://ci.example"}, path, fixtureHostKeys)
	if err == nil || publicerr.Classify(err).Reason() != "Invitation refused." {
		t.Fatal("expected the known refusal", err)
	}
	if !pinned {
		t.Error("written handoff was not held open during the request")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(replacement, after) {
		t.Fatal("refusal removed or changed the recreated file", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "unrelated user file" {
		t.Fatal("refusal changed the recreated contents", err)
	}
	if handoffDescriptorOpen(t, original) {
		t.Fatal("handoff descriptor remains open after cleanup")
	}
}

func recreateDeletedHandoff(t *testing.T, path string, original os.FileInfo) os.FileInfo {
	t.Helper()
	var candidate os.FileInfo
	for attempt := range 500 {
		if err := os.WriteFile(path, []byte("unrelated user file"), 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		candidate = info
		if os.SameFile(original, info) || attempt == 499 {
			break
		}
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	return candidate
}

func handoffDescriptorOpen(t *testing.T, original os.FileInfo) bool {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	identity := original.Sys().(*syscall.Stat_t)
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		var current syscall.Stat_t
		if syscall.Fstat(fd, &current) == nil && current.Dev == identity.Dev && current.Ino == identity.Ino {
			return true
		}
	}
	return false
}
