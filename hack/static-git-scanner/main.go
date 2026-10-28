// Run only the pinned static-component scan with a closed environment/argv.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/staticgit"
)

func run() int {
	return runScanner(os.Args[1:])
}

// scan-static-git.sh verifies the pinned archive before extracting grype into
// a private directory. Bind execution to its regular executable descriptor,
// confined beneath that directory, with fixed argv and no shell or PATH lookup.
func runScanner(arguments []string) int {
	if len(arguments) != 5 {
		fmt.Fprintln(os.Stderr, "usage: scanner-driver GRYPE CONFIG CACHE SBOM DISTRO")
		return 1
	}
	scanner, config, cache, sbom, distro := arguments[0], arguments[1], arguments[2], arguments[3], arguments[4]
	for _, path := range []string{scanner, config, cache, sbom} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return 1
		}
	}
	if distro != "alpine:"+staticgit.Inputs()["ALPINE_VERSION"] || filepath.Base(scanner) != "grype" {
		return 1
	}
	root, err := os.OpenRoot(filepath.Dir(scanner))
	if err != nil {
		return 1
	}
	defer func() { _ = root.Close() }()
	binary, err := root.Open("grype")
	if err != nil {
		return 1
	}
	defer func() { _ = binary.Close() }()
	info, err := binary.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "/proc/self/fd/3", "--config", config, "sbom:"+sbom, "--distro", distro, "--fail-on", "high", "-o", "json") // #nosec G204 G702 -- fixed descriptor of the confined, archive-verified scanner; metadata paths are absolute argv values after fixed flags, never a shell command.
	command.ExtraFiles = []*os.File{binary}
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(scanner), "GRYPE_DB_CACHE_DIR=" + cache, "GRYPE_CHECK_FOR_APP_UPDATE=false", "GRYPE_DB_AUTO_UPDATE=true"}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if value := os.Getenv(key); value != "" {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	err = command.Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 2 {
		return 2
	}
	fmt.Fprintln(os.Stderr, "static component scanner failed")
	return 1
}
func main() { os.Exit(run()) }
