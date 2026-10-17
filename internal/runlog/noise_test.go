package runlog

import "testing"

func TestIsExecutorNoise(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`[setup/install-helm] time=2026-10-17T16:29:39.819Z level=INFO msg="sub-process exited" argo=true error=<nil>`, true},
		{`[setup/install-helm] time=2026-10-17T16:29:39.819Z level=INFO msg="file signal handler exiting due to context cancellation" argo=true`, true},
		{`[setup/fetch-shellcheck] time=2026-10-17T16:29:54.886Z level=INFO msg="waiting for signals" argo=true signalPath=/var/run/argo/ctr/main/signal`, true},
		{`time=2026-10-17T16:29:54.886Z level=INFO msg="capturing logs" argo=true`, true},
		{`time=2026-10-17T16:29:54.886Z level=debug msg="Loaded container args from file" argo=true`, true},
		{`[setup/verify-shellcheck] /tmp/oberth-tools/shellcheck-v0.10.0.linux.x86_64.tar.xz: OK`, false},
		{`time=2026-10-17T16:29:39.819Z level=INFO msg="sub-process exited" argo=true error="exit status 1"`, false},
		{`time=2026-10-17T16:29:39.819Z level=WARN msg="cannot save parameter, does not exist" argo=true`, false},
		{`time=2026-10-17T16:29:39.819Z level=ERROR msg="failed to write exit code" argo=true`, false},
		{`echo argo=true`, false},
		{`msg="waiting for signals" argo=true`, false},
		{`time=2026-10-17T16:29:39.819Z level=INFO msg="sub-process exited" argo=trueish`, false},
		{`[x] time=2026-10-17T16:29:39.819Z level=INFO msg="waiting for signals" argo=true`, false},
		{`time=2026-10-17T16:29:39.819Z level=INFO msg=x argo=true error=<nil> err="boom"`, false},
		{``, false},
	} {
		if got := IsExecutorNoise(tc.line); got != tc.want {
			t.Errorf("IsExecutorNoise(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
