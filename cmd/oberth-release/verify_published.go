package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func verifyPublished(ctx context.Context, o options, release *releaseverify.Release) error {
	record := release.Record()
	if err := checkPublishedImages(ctx, release, ""); err != nil {
		return err
	}
	staged, err := os.MkdirTemp("", "verified-cli-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staged) }()
	executable := filepath.Join(staged, "oberth")
	input, err := release.Open("oberth-linux-" + runtime.GOARCH)
	if err != nil {
		return err
	}
	if err := stageFile(input, executable); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, executable, "version").Output()
	if err != nil || !versionMatches(output, record.Version) {
		return binariesMismatch
	}
	return writeVerification(ctx, o, record)
}

func versionMatches(output []byte, version string) bool {
	fields := strings.Fields(string(output))
	return len(fields) == 4 && fields[0] == "oberth" && fields[1] == version && strings.HasPrefix(fields[2], "commit=") && strings.HasPrefix(fields[3], "date=")
}
