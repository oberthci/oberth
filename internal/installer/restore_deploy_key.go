package installer

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

func restoreDeployPublicPath(server serverDeployment) string {
	path := "/run/oberth-identities/upstream/id_ed25519"
	for _, container := range server.PodSpec.Containers {
		if container.Name != serverContainerName {
			continue
		}
		for i, arg := range container.Args {
			if value, ok := strings.CutPrefix(arg, "--upstream-key="); ok {
				path = value
			}
			if arg == "--upstream-key" && i+1 < len(container.Args) {
				path = container.Args[i+1]
			}
		}
	}
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") {
		return ""
	}
	return path + ".pub"
}

func readRestoreDeployPublicKey(ctx context.Context, deps Deps, namespace, pod, path string) string {
	if path == "" || pod == "" || deps.RunCommand == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := deps.RunCommand(ctx, nil, "kubectl", kubectlContextArgs(deps.ContextName, "exec", "-n", namespace, pod, "-c", serverContainerName, "--", "/usr/local/bin/oberth", "admin", "read-file", path)...)
	defer clear(raw)
	if err != nil || len(raw) > 16384 {
		return ""
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey(raw)
	if err != nil || len(strings.TrimSpace(string(rest))) > 0 {
		return ""
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func printRestoreDeployKey(w io.Writer, cfg RestoreConfig, target restoreTarget) {
	_, _ = fmt.Fprintln(w, "\nForge access: the deploy key is an install identity and is not restored from the backup.")
	if target.DeployPublicKey != "" {
		_, _ = fmt.Fprintln(w, "Add this installation's public key to every restored repository's forge Settings → Deploy keys, with write access:")
		_, _ = fmt.Fprintln(w, target.DeployPublicKey)
	} else {
		_, _ = fmt.Fprintln(w, "The current public deploy key could not be read. For each restored SSH upstream, run oberth upstream add NAME BASE_URL with the unchanged registration; it generates or prints this install's public key. Add that key to the forge's deploy keys with write access.")
	}
	args := kubectlContextArgs(cfg.ContextName, "exec", "-n", cfg.Namespace, "deploy/oberth", "-c", serverContainerName, "--", "/usr/local/bin/oberth", "upstream", "list", "--check")
	for i, arg := range args {
		args[i] = shellQuoteWord(arg)
	}
	_, _ = fmt.Fprintf(w, "Then verify forge authentication and repository access:\n    kubectl %s\n", strings.Join(args, " "))
	_, _ = fmt.Fprintln(w, "Regenerate and register each dedicated upstream key separately with its original --dedicated-key setting. Re-enroll certificate clients against this install's client CA.")
}
