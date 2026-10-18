package setuptui

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// releasePresent reports whether Helm release oberth exists in namespace on
// the saved cluster; known is false when the cluster could not be asked.
// Tests replace it.
var releasePresent = func(ctx context.Context, kubeconfig, kubeContext, namespace string) (present, known bool) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"status", "oberth", "-n", namespace}
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	// #nosec G204 -- fixed executable and structured arguments.
	out, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
	switch {
	case err == nil:
		return true, true
	case bytes.Contains(out, []byte("release: not found")):
		return false, true
	}
	return false, false
}

// forgetVanishedInstall drops a saved "install complete" (or partial) record
// when the release it describes is gone, as after a cluster reinstall: the
// answers stay, the Review and Install steps open again (#069). It returns
// the notice to show, or "".
func forgetVanishedInstall(ctx context.Context, st *WizardState) string {
	if st.Install.Status == installNone {
		return ""
	}
	present, known := releasePresent(ctx, st.Config.KubeconfigPath, st.Config.ContextName, st.namespace())
	if present || !known {
		return ""
	}
	at := ""
	if !st.Install.At.IsZero() {
		at = " (" + st.Install.At.Local().Format("2006-01-02 15:04") + ")"
	}
	st.Install = installRecord{}
	st.done[stepReview] = false
	st.done[stepInstall] = false
	return "Your previous install" + at + " is no longer on this cluster: its answers are kept; review them and install again."
}
