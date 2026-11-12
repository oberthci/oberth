package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const swapRemedy = "On the node, install zram-tools, set PERCENT=25 in /etc/default/zramswap and run sudo systemctl enable --now zramswap; keep kubelet failSwapOn: false and memorySwap.swapBehavior: NoSwap so host swap protects system daemons without swapping pipeline Pods."
const inotifyRemedy = "On the node, write fs.inotify.max_user_instances=1024 and fs.inotify.max_user_watches=1048576 to /etc/sysctl.d/90-oberth.conf, then run sudo sysctl --system. Verify with sysctl fs.inotify.max_user_instances fs.inotify.max_user_watches."
const kubeletRemedy = "Set systemReserved: {cpu: 500m, memory: 1Gi} and evictionHard: {memory.available: 500Mi, nodefs.available: 10%, imagefs.available: 15%} in the node's KubeletConfiguration, then restart kubelet. For k3s, add kubelet-arg: [system-reserved=cpu=500m,memory=1Gi, eviction-hard=memory.available<500Mi,nodefs.available<10%,imagefs.available<15%] to /etc/rancher/k3s/config.yaml (quote each list item), then sudo systemctl restart k3s."

// Host checks are advisory: no node mutation or privileged probe Pod. Never
// inspect the install client's kernel as if it belonged to a remote node.
func inspectHostPrep(ctx context.Context, cfg Config, deps Deps, node corev1.Node, r *InstallResult) {
	host, _ := os.Hostname()
	local := runtime.GOOS == "linux" && (host == node.Name || host == node.Labels["kubernetes.io/hostname"])
	var swap, instances, watches []byte
	if local {
		swap, _ = os.ReadFile("/proc/swaps")
		instances, _ = os.ReadFile("/proc/sys/fs/inotify/max_user_instances")
		watches, _ = os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	}
	addHostPrepWarnings(node.Name, swap, instances, watches, r)
	args := []string{"--context", deps.ContextName, "get", "--raw", "/api/v1/nodes/" + url.PathEscape(node.Name) + "/proxy/configz"}
	if cfg.KubeconfigPath != "" {
		args = append([]string{"--kubeconfig", cfg.KubeconfigPath}, args...)
	}
	raw, err := deps.RunCommand(ctx, nil, "kubectl", args...)
	var config struct {
		KubeletConfig struct {
			SystemReserved map[string]string `json:"systemReserved"`
			EvictionHard   map[string]string `json:"evictionHard"`
		} `json:"kubeletconfig"`
	}
	ok := err == nil && json.Unmarshal(raw, &config) == nil && config.KubeletConfig.SystemReserved["cpu"] != "" && config.KubeletConfig.SystemReserved["memory"] != "" && config.KubeletConfig.EvictionHard["memory.available"] != ""
	hostPrepWarning(r, "host_kubelet/"+node.Name, ok, kubeletRemedy)
}
func addHostPrepWarnings(node string, swap, instances, watches []byte, r *InstallResult) {
	swapOK := len(strings.Fields(string(swap))) > 5
	i, _ := strconv.ParseInt(strings.TrimSpace(string(instances)), 10, 64)
	w, _ := strconv.ParseInt(strings.TrimSpace(string(watches)), 10, 64)
	hostPrepWarning(r, "host_swap/"+node, swapOK, swapRemedy)
	hostPrepWarning(r, "host_inotify/"+node, i >= 1024 && w >= 1048576, inotifyRemedy)
}
func hostPrepWarning(r *InstallResult, code string, ok bool, remedy string) {
	if ok {
		r.Checks = append(r.Checks, CheckResult{Code: code, OK: true})
		return
	}
	r.Checks = append(r.Checks, CheckResult{Code: code, OK: false, Warning: true, Reason: "warning: missing or unverified host preparation", Remedy: fmt.Sprintf("%s Inspect on the selected node if remote; see docs/host-preparation.md.", remedy)})
}
