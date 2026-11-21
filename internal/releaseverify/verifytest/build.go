package verifytest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func runFixture(ctx context.Context, key []byte) error {
	directory := os.Getenv("GOTMPDIR")
	if !filepath.IsAbs(directory) {
		return errors.New("fixture build requires absolute GOTMPDIR")
	}
	directory, err := os.MkdirTemp(directory, "oberth-release-test-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	parentArgs, err := parentTestArguments()
	if err != nil {
		return err
	}
	files, err := fixtureOverlay(ctx, directory, key, parentArgs)
	if err != nil {
		return err
	}
	binary := filepath.Join(directory, "oberth-fixture.test")
	args := []string{"test", "-c", "-p=1", "-buildvcs=false", "-timeout=" + flagTimeout(), "-overlay=" + files.overlay, "-o", binary}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, ".")
	if err = fixtureCommand(ctx, files.goTool, args).Run(); err != nil {
		return err
	}
	command := fixtureCommand(ctx, binary, os.Args[1:])
	command.Env = append(os.Environ(), "GOFLAGS="+files.goFlags)
	return command.Run()
}

type fixtureFiles struct{ overlay, goTool, goFlags string }

func fixtureOverlay(ctx context.Context, directory string, key []byte, parentArgs []string) (fixtureFiles, error) {
	command := exec.CommandContext(ctx, "go", "env", "-json", "GOMOD", "GOFLAGS", "GOROOT")
	command.WaitDelay = 5 * time.Second
	raw, err := command.Output()
	if err != nil {
		return fixtureFiles{}, err
	}
	var environment struct{ GOMOD, GOFLAGS, GOROOT string }
	if err = json.Unmarshal(raw, &environment); err != nil {
		return fixtureFiles{}, err
	}
	replacements, err := callerOverlay(ctx, environment.GOFLAGS, parentArgs)
	if err != nil {
		return fixtureFiles{}, err
	}
	// Nested product builds inherit caller mutations, never the test signing pin.
	caller := filepath.Join(directory, "caller.json")
	if err := writeOverlay(caller, replacements); err != nil {
		return fixtureFiles{}, err
	}
	flag, err := overlayFlag(caller)
	if err != nil {
		return fixtureFiles{}, err
	}
	fixture := filepath.Join(directory, "fixture.pem")
	if err = os.WriteFile(fixture, key, 0600); err != nil {
		return fixtureFiles{}, err
	}
	replacements[filepath.Join(filepath.Dir(environment.GOMOD), "internal", "releaseverify", "release-cosign.pub")] = fixture
	path := filepath.Join(directory, "overlay.json")
	files := fixtureFiles{
		overlay: path,
		goTool:  filepath.Join(environment.GOROOT, "bin", "go"),
		goFlags: environment.GOFLAGS + " " + flag,
	}
	return files, writeOverlay(path, replacements)
}

func writeOverlay(path string, replacements map[string]string) error {
	raw, err := json.Marshal(map[string]any{"Replace": replacements})
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}
