// Package kubeconfig keeps operator configuration read-only, including client-go
// migration and credential persistence performed by child tools.
package kubeconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func Rules(path string) *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.MigrationRules = nil
	rules.ExplicitPath = path
	return rules
}

func Read(rules *clientcmd.ClientConfigLoadingRules) (*clientcmdapi.Config, error) {
	copy := *rules
	copy.MigrationRules = nil
	copy.DoNotResolvePaths = false
	return copy.Load()
}

func Client(rules *clientcmd.ClientConfigLoadingRules, contextName string) (*clientcmdapi.Config, *rest.Config, error) {
	raw, err := Read(rules)
	if err != nil {
		return nil, nil, err
	}
	if contextName != "" {
		raw.CurrentContext = contextName
	}
	client := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, &clientcmd.ConfigOverrides{}, nil)
	config, err := client.ClientConfig()
	if err != nil {
		return nil, nil, err
	}
	config.AuthConfigPersister = nil
	return raw, config, nil
}

// Command gives kubectl/Helm a private merge through both flags and environment.
// A private HOME prevents client-go's migration from touching an ambient config,
// even though that migration runs before an explicit --kubeconfig is resolved.
// Exec credential plugins retain their original HOME and may own their caches.
func Command(name string, args, environment []string) ([]string, []string, *os.File, func() error, error) {
	cleanup := func() error { return nil }
	base := filepath.Base(name)
	if base != "kubectl" && base != "helm" {
		return args, environment, nil, cleanup, nil
	}
	path, selected, forwarded, err := commandTarget(args)
	if err != nil {
		return nil, nil, nil, cleanup, err
	}
	raw, err := Read(Rules(path))
	if err != nil {
		return nil, nil, nil, cleanup, fmt.Errorf("read selected kubeconfig: %w", err)
	}
	if selected != "" {
		raw.CurrentContext = selected
	}
	originalHome, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, nil, cleanup, err
	}
	for _, auth := range raw.AuthInfos {
		auth.LocationOfOrigin = ""
		if auth.Exec != nil {
			hasHome := false
			for _, entry := range auth.Exec.Env {
				hasHome = hasHome || entry.Name == "HOME"
			}
			if !hasHome {
				auth.Exec.Env = append(auth.Exec.Env, clientcmdapi.ExecEnvVar{Name: "HOME", Value: originalHome})
			}
		}
	}
	for _, cluster := range raw.Clusters {
		cluster.LocationOfOrigin = ""
	}
	for _, context := range raw.Contexts {
		context.LocationOfOrigin = ""
	}
	directory, lease, cleanup, err := newPrivateCopy()
	if err != nil {
		return nil, nil, nil, cleanup, err
	}
	destination := filepath.Join(directory, "config")
	data, err := clientcmd.Write(*raw)
	if err == nil {
		err = os.WriteFile(destination, data, 0600)
	}
	clear(data)
	if err != nil {
		return nil, nil, nil, func() error { return nil }, errors.Join(err, cleanup())
	}
	env := preserveToolDirectories(environment)
	env = replaceEnvironment(env, "HOME", directory)
	env = replaceEnvironment(env, "KUBECONFIG", destination)
	return append([]string{"--kubeconfig", destination}, forwarded...), env, lease, cleanup, nil
}

// Only parse host flags; arguments after -- belong to a remote command.
func commandTarget(args []string) (path, selected string, forwarded []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			forwarded = append(forwarded, args[i:]...)
			break
		}
		key, value, inline := strings.Cut(arg, "=")
		if key != "--kubeconfig" && key != "--context" && key != "--kube-context" {
			forwarded = append(forwarded, arg)
			continue
		}
		if !inline {
			i++
			if i == len(args) {
				return "", "", nil, fmt.Errorf("%s requires a value", key)
			}
			value = args[i]
		}
		if key == "--kubeconfig" {
			path = value
			continue
		}
		selected = value
		forwarded = append(forwarded, key, value)
	}
	return
}

func replaceEnvironment(env []string, key, value string) []string {
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func preserveToolDirectories(env []string) []string {
	// Keep Helm's existing repositories/plugins when isolating kubeconfig HOME.
	config, _ := os.UserConfigDir()
	cache, _ := os.UserCacheDir()
	home, _ := os.UserHomeDir()
	data := filepath.Join(home, ".local", "share")
	configBase := config
	if runtime.GOOS == "darwin" {
		data = filepath.Join(home, "Library")
		configBase = filepath.Join(home, "Library", "Preferences")
	}
	for key, target := range map[string]*string{"XDG_CONFIG_HOME": &configBase, "XDG_CACHE_HOME": &cache, "XDG_DATA_HOME": &data} {
		if value := os.Getenv(key); value != "" {
			*target = value
		}
	}
	defaults := map[string]string{"HELM_CONFIG_HOME": filepath.Join(configBase, "helm"), "HELM_CACHE_HOME": filepath.Join(cache, "helm"), "HELM_DATA_HOME": filepath.Join(data, "helm")}
	for key, value := range defaults {
		exists := false
		for _, entry := range env {
			exists = exists || strings.HasPrefix(entry, key+"=")
		}
		if !exists {
			env = append(env, key+"="+value)
		}
	}
	return env
}
