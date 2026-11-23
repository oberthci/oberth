package installer_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

// This validation mirrors C11's config and runtime checks until its real loader lands.
func validateChartValues(c chartServerConfig) error {
	checks := []struct {
		field string
		valid bool
	}{
		{"version", c.Version == 1},
		{"namespace", c.Namespace != ""},
		{"argo_namespace", c.ArgoNamespace != ""},
		{"pipeline_namespace", c.PipelineNamespace != "" && c.PipelineNamespace != c.Namespace},
		{"public.url", chartHTTPSURL(c.Public.URL)},
		{"public.ssh_port", c.Public.SSHPort > 0 && c.Public.SSHPort <= 65535},
		{"database_file", filepath.IsAbs(c.DatabaseFile)},
		{"protocol", c.Protocol.Validate() == nil},
		{"secret_store.address", chartHTTPSURL(c.SecretStore.Address)},
		{"secret_store.audience", c.SecretStore.Audience != "" && strings.TrimSpace(c.SecretStore.Audience) == c.SecretStore.Audience},
		{"secret_store.kv_mount", c.SecretStore.KVMount != ""},
		{"secret_store.auth_mount", c.SecretStore.AuthMount != ""},
		{"secret_store.writer_role", c.SecretStore.WriterRole != ""},
		{"logs.run_limit_bytes", c.Logs.RunLimitBytes > 0},
		{"schedules.max_entries", c.Schedules.MaxEntries >= 0 && c.Schedules.MaxEntries <= 256},
		{"assist.ttl_seconds", c.Assist.TTLSeconds > 0},
	}
	for _, check := range checks {
		if !check.valid {
			return fmt.Errorf("%s: invalid value", check.field)
		}
	}
	for _, address := range []string{c.Listen.HTTPS, c.Listen.SSH, c.Listen.Source, c.Listen.Health} {
		if _, _, err := net.SplitHostPort(address); err != nil {
			return fmt.Errorf("listen: %w", err)
		}
	}
	return validateChartRuntime(c.Runtime)
}

func validateChartRuntime(r chartServerRuntime) error {
	if !filepath.IsAbs(r.GitBinary) || r.SourceTimeoutSeconds < 1 || r.SourceTimeoutSeconds > 3600 || r.AdmissionTimeoutSeconds < 1 || r.AdmissionTimeoutSeconds > 3600 || r.SourceClientBurst < 3 {
		return fmt.Errorf("runtime: invalid Git path, duration or client burst")
	}
	if !chartDefaultBranch(r.DefaultBranch) {
		return fmt.Errorf("runtime: invalid default branch")
	}
	for _, seconds := range []int{r.IssueStaleSeconds, r.IssueActiveStaleSeconds} {
		if seconds < 60 || seconds > 365*24*3600 {
			return fmt.Errorf("runtime: invalid issue stale window")
		}
	}
	return r.Routes.Validate()
}

func chartHTTPSURL(text string) bool {
	u, err := url.Parse(text)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

// The test-local public-ref check follows sourcecontract.ValidatePublicRef.
func chartDefaultBranch(ref string) bool {
	suffix, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || len(suffix) < 1 || len(suffix) > 255 || !chartRefSyntax(ref) {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") || strings.HasSuffix(part, ".") {
			return false
		}
	}
	return true
}

func chartRefSyntax(ref string) bool {
	if strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.ContainsAny(ref, " ~^:?*[\\") {
		return false
	}
	for _, c := range ref {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func uniqueChartJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	names := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name := key.(string)
			if names[name] {
				return fmt.Errorf("duplicate config key %s", name)
			}
			names[name] = true
		}
		if err := uniqueChartJSON(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}
