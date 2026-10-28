package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/staticgit"
)

func run() error {
	apkRoot := flag.String("apk-root", "/", "exact APK toolchain root")
	binary := flag.String("git", "", "freshly built static Git path")
	patch := flag.String("patch", "hack/static-git-no-shell.patch", "source patch")
	arch := flag.String("arch", "amd64", "artifact architecture")
	sbom := flag.String("sbom", "", "CycloneDX output path")
	control := flag.Bool("vulnerable-control", false, "emit old-Git scanner coverage control")
	flag.Parse()
	if *binary != "" {
		git, err := os.ReadFile(*binary)
		if err != nil {
			return err
		}
		musl, err := os.ReadFile(filepath.Join(*apkRoot, "usr/lib/libc.a"))
		if err != nil {
			return err
		}
		zlib, err := os.ReadFile(filepath.Join(*apkRoot, "usr/lib/libz.a"))
		if err != nil {
			return err
		}
		installed, err := os.ReadFile(filepath.Join(*apkRoot, "lib/apk/db/installed"))
		if err != nil {
			return err
		}
		if err := staticgit.VerifyBuild(*arch, git, musl, zlib, string(installed)); err != nil {
			return err
		}
		patchBytes, err := os.ReadFile(*patch)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(patchBytes)
		if hex.EncodeToString(hash[:]) != staticgit.Inputs()["GIT_PATCH_SHA256"] {
			return fmt.Errorf("static Git patch does not match its pin")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, *binary, "--version") // #nosec G204 -- explicit CI artifact, byte-verified above.
		version, err := command.Output()
		if err != nil {
			return err
		}
		if string(version) != "git version "+staticgit.Inputs()["GIT_VERSION"]+"\n" {
			return fmt.Errorf("static Git version differs from its component record")
		}
	}
	if *sbom == "" {
		return fmt.Errorf("--sbom output is required")
	}
	output, err := os.Create(*sbom)
	if err != nil {
		return err
	}
	result := staticgit.WriteSBOM(output, *arch, *control)
	closed := output.Close()
	if result != nil {
		return result
	}
	return closed
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
