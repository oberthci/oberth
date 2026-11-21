package client

import (
	"errors"
	"github.com/oberthci/oberth/internal/publicerr"
	"os"
	"path/filepath"
)

func stageJoin(dir string) (string, error) {
	base := filepath.Dir(dir)
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", fileFailure(base, "write", err)
	}
	root, err := directory(base, true)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if _, err = root.Lstat(filepath.Base(dir)); !errors.Is(err, os.ErrNotExist) {
		problem := failure("profile_exists", err)
		problem.Context = []string{"Profile: " + filepath.Base(dir)}
		return "", problem
	}
	stage, err := os.MkdirTemp(base, ".join-")
	if err != nil {
		return "", fileFailure(base, "write", err)
	}
	return stage, nil
}
func cleanupJoin(dir string) {
	for _, name := range []string{"admin.key", "admin.csr", UplinkPushKeyFile, "push.pub", HostKeysFile, "admin.crt", "ca.crt", "installation.json"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	_ = os.Remove(dir)
}
func joinRequestFailure(err error, wrote bool, address string) error {
	var public *publicerr.Error
	if errors.As(err, &public) && public.Reason() == catalogue()["join_peer"][0] {
		return public
	}
	if wrote {
		return failure("join_unknown", err)
	}
	problem := failure("join_connect", err)
	problem.Detail = "Server: " + address
	return problem
}
