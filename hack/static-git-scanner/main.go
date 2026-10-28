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
)

func run() int {
	if len(os.Args) != 6 {
		fmt.Fprintln(os.Stderr, "usage: scanner-driver GRYPE CONFIG CACHE SBOM DISTRO")
		return 1
	}
	scanner, config, cache, sbom, distro := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]
	if !filepath.IsAbs(scanner) || !filepath.IsAbs(config) || !filepath.IsAbs(cache) || !filepath.IsAbs(sbom) {
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, scanner, "--config", config, "sbom:"+sbom, "--distro", distro, "--fail-on", "high", "-o", "json") // #nosec G204 -- private checksum-verified scanner, fixed flags, explicit metadata paths.
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Dir(scanner), "GRYPE_DB_CACHE_DIR=" + cache, "GRYPE_CHECK_FOR_APP_UPDATE=false", "GRYPE_DB_AUTO_UPDATE=true"}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if value := os.Getenv(key); value != "" {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	err := command.Run()
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
