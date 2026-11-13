package installer

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/crypto/ssh"
)

// InstallUpstream contains only public registration and follow-up information.
// Pending means registered locally, with forge authorization still to verify.
type InstallUpstream struct {
	Name             string `json:"name"`
	URL              string `json:"url"`
	DeployPublicKey  string `json:"deploy_public_key"`
	Fingerprint      string `json:"fingerprint"`
	ForgeSettingsURL string `json:"forge_settings_url"`
	State            string `json:"state"`
	NextCommand      string `json:"next_command"`
}

func installUpstreamResult(cfg Config, deps Deps, name, baseURL, public string, verified bool) InstallUpstream {
	result := InstallUpstream{Name: name, URL: baseURL, State: "pending", ForgeSettingsURL: forgeKeySettingsURL(baseURL)}
	if verified {
		result.State = "verified"
	}
	if key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(public)); err == nil && len(strings.TrimSpace(string(rest))) == 0 {
		result.DeployPublicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		result.Fingerprint = ssh.FingerprintSHA256(key)
	}
	args := cfg.upstreamAddArgs(name, baseURL)
	var verifyArgs []string
	for _, arg := range args {
		if arg != "--no-wait" {
			verifyArgs = append(verifyArgs, arg)
		}
	}
	result.NextCommand = printedKubectl(cfg, kubectlOberthArgs(cfg, deps, false, verifyArgs...))
	return result
}

// An organization has no repository deploy-key page. These links are the
// machine user's SSH-key settings; repository-specific setup uses add-repo.
func forgeKeySettingsURL(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	switch parsed.Hostname() {
	case "github.com":
		return "https://github.com/settings/keys"
	case "gitlab.com":
		return "https://gitlab.com/-/user_settings/ssh_keys"
	case "codeberg.org":
		return "https://codeberg.org/user/settings/keys"
	default:
		return ""
	}
}

func upstreamKeyRemedy(upstream InstallUpstream) string {
	if upstream.ForgeSettingsURL != "" {
		return fmt.Sprintf("Add this key at %s while signed in as a machine user with write access to the organization repositories, or use /oberth:add-repo to add a specific repository deploy key with write access. Then run the verification command.", upstream.ForgeSettingsURL)
	}
	return "Add this public key to the forge as a machine-user SSH key or a repository deploy key with write access, then run the verification command."
}

func (r *InstallResult) checkUpstreamPending(upstream InstallUpstream) {
	r.check("upstream_key_pending", false, upstreamKeyRemedy(upstream))
	r.Checks[len(r.Checks)-1].DeployPublicKey = upstream.DeployPublicKey
}

func installResultError(result InstallResult) error {
	if result.OK {
		return nil
	}
	pending := false
	for _, check := range result.Checks {
		if check.OK || check.Warning {
			continue
		}
		if check.Code != "upstream_key_pending" {
			if check.Reason == "" {
				return fmt.Errorf("%s: %s", check.Code, check.Remedy)
			}
			return fmt.Errorf("%s: %s; %s", check.Code, check.Reason, check.Remedy)
		}
		pending = true
	}
	if pending {
		return ErrUpstreamKeyPending
	}
	return fmt.Errorf("install did not pass all checks; follow the reported remedies")
}
