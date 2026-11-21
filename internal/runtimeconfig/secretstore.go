package runtimeconfig

import (
	"crypto/x509"
	"net/url"
	"regexp"
)

// SecretStoreAccess is the static public configuration shared by server and step clients.
type SecretStoreAccess struct {
	Mount             string `json:"mount"`
	AuthMount         string `json:"auth_mount"`
	WriterRole        string `json:"writer_role"`
	StepRole          string `json:"step_role"`
	ServerNamespace   string `json:"server_namespace"`
	PipelineNamespace string `json:"pipeline_namespace"`
	ServerAccount     string `json:"server_account"`
	Audience          string `json:"audience"`
}

// SecretDelivery contains only public store connection facts, never a token or value.
// The server reads CAPEM from its configured store CA mount.
type SecretDelivery struct {
	Address    string            `json:"address"`
	ServerName string            `json:"server_name"`
	CAPEM      string            `json:"ca_pem"`
	Access     SecretStoreAccess `json:"access"`
}

const (
	SecretConfigEnv        = "OBERTH_SECRETSTORE_CONFIG"
	SecretPathsEnv         = "OBERTH_SECRETSTORE_PATHS"
	SecretTokenEnv         = "OBERTH_SECRETSTORE_TOKEN_FILE"
	SecretScratchEnv       = "OBERTH_SECRETSTORE_SCRATCH"
	SecretTokenDirectory   = "/run/oberth/store"
	SecretScratchDirectory = "/run/oberth/secrets"
)

const SecretCABytes = 64 << 10

func ValidStoreName(value string) bool {
	return regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`).MatchString(value)
}
func (c SecretStoreAccess) Valid() bool {
	for _, value := range []string{c.Mount, c.AuthMount, c.WriterRole, c.StepRole, c.ServerNamespace, c.PipelineNamespace, c.ServerAccount, c.Audience} {
		if !ValidStoreName(value) {
			return false
		}
	}
	return c.ServerNamespace != c.PipelineNamespace && c.WriterRole != c.StepRole
}
func ValidStoreAddress(address string) bool {
	u, err := url.Parse(address)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}
func (d SecretDelivery) Valid() bool {
	return ValidStoreAddress(d.Address) && d.Access.Valid() && d.ServerName != "" && len(d.CAPEM) <= SecretCABytes && x509.NewCertPool().AppendCertsFromPEM([]byte(d.CAPEM))
}

func SecretStoreConfiguration() (string, string) {
	return "Secret store is not configured.", "An administrator runs oberth secretstore check."
}
