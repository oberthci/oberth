package installer

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

func readRestoreDeployPublicKey(ctx context.Context, deps Deps, namespace string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := readPublicIdentity(ctx, Config{Namespace: namespace}, deps, "upstream-public-key")
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
	_, _ = fmt.Fprintln(w, "Then verify forge authentication and repository access:\n    oberth upstream list --check")
	_, _ = fmt.Fprintln(w, "Regenerate and register each dedicated upstream key separately with its original --dedicated-key setting. Re-enroll certificate clients against this install's client CA.")
}
