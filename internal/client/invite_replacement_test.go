package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestReviewRefusedInvitePreservesReplacement(t *testing.T) {
	for _, shape := range []string{"file", "directory", "symlink", "parent-renamed"} {
		t.Run(shape, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "delivery")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "invite.json")
			var replacement os.FileInfo
			h := &fake.Handler[apicontract.UplinkInviteArgs, model.Invitation]{Run: func(context.Context) (model.Invitation, error) {
				replacement = replaceInviteHandoff(t, path, shape)
				return model.Invitation{}, publicerr.New(publicerr.InvalidInput, "Invitation refused.", "Correct the invitation and retry.", nil)
			}}
			b := fake.Bindings()
			b.UplinkInvite.Handler = h
			c := testClient(t, handlerTransport(server(t, b)), nil)
			defer c.Close()
			_, err := c.Invite(t.Context(), apicontract.UplinkInviteArgs{Identity: "agent", ServerURL: "https://ci.example"}, path, fixtureHostKeys)
			if err == nil {
				t.Fatal("expected known refusal")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(replacement, after) {
				t.Fatal("refusal changed the replacement", err)
			}
			if shape == "parent-renamed" {
				if _, err = os.Lstat(filepath.Join(filepath.Dir(parent), "moved-delivery", "invite.json")); !os.IsNotExist(err) {
					t.Fatal("owned handoff in the opened parent was not removed", err)
				}
			} else if _, err = ReadPrivate(filepath.Join(parent, "moved-invitation")); err != nil {
				t.Fatal("moved handoff changed", err)
			}
		})
	}
}

func replaceInviteHandoff(t *testing.T, path, shape string) os.FileInfo {
	t.Helper()
	parent := filepath.Dir(path)
	if shape == "parent-renamed" {
		if err := os.Rename(parent, filepath.Join(filepath.Dir(parent), "moved-delivery")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Rename(path, filepath.Join(parent, "moved-invitation")); err != nil {
		t.Fatal(err)
	}
	var err error
	switch shape {
	case "directory":
		err = os.Mkdir(path, 0700)
	case "symlink":
		err = os.Symlink("moved-invitation", path)
	default:
		err = os.WriteFile(path, []byte("unrelated user file"), 0600)
	}
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
