package installer

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/secretstore"
)

const (
	// defaultServerPKIMount is the PKI secrets engine holding the server CA.
	defaultServerPKIMount = "oberth-pki"
	// defaultServerPKIRole issues the server's short-lived leaves.
	defaultServerPKIRole = "oberth-server"
	// defaultServerLeafTTL is the server leaf lifetime; it renews at 2/3.
	defaultServerLeafTTL = "720h"
	// serverCAValidity is the CA lifetime; clients pin the CA.
	serverCAValidity = 10 * 365 * 24 * time.Hour
	// serverCARenewBefore re-issues a CA that would expire within this window.
	serverCARenewBefore = 365 * 24 * time.Hour
)

// serverPKIPolicy lets the server request its own leaf and nothing else in
// the PKI mount: no CA reads beyond the issue response, no other roles.
func serverPKIPolicy() string {
	return fmt.Sprintf(`

# Short-lived server certificates from the name-constrained server CA.
path "%s/issue/%s" {
  capabilities = ["update"]
}`, defaultServerPKIMount, defaultServerPKIRole)
}

// serverPKINames are the names the CA permits and the role issues: the
// server's in-cluster and goproxy names plus every operator-supplied address.
func serverPKINames(cfg Config) (dnsNames, ips []string) {
	ns := cfg.Namespace
	if ns == "" {
		ns = DefaultNamespace
	}
	return secretstore.ServerTLSNames(ns, append(append([]string(nil), cfg.TLSExtraDNSNames...), cfg.TLSExtraIPs...))
}

// ensureServerPKI provisions the server CA and issuing role and returns the
// CA certificate clients pin.
//
// OpenBao generates the CA (type internal), so its key never exists outside
// OpenBao. The CA is name-constrained to the server's DNS names. It carries
// no IP constraints: Claude Code's TLS stack (BoringSSL) rejects any chain
// whose CA has them (UNSUPPORTED_CONSTRAINT_TYPE). IP SANs are limited at
// issuance instead: the role allows only the configured addresses and only
// the server's policy may use the role. A CA that no longer covers the DNS
// names, or is close to expiry, is replaced, which changes what clients pin;
// the installer refreshes the pins it manages itself.
func ensureServerPKI(ctx context.Context, cfg Config, store openBaoExec, token string, mounts map[string]baoMount) (caPEM string, replaced bool, err error) {
	if mount, exists := mounts[defaultServerPKIMount+"/"]; exists {
		if mount.Type != "pki" {
			return "", false, fmt.Errorf("mount %s already exists with type %q, not pki", defaultServerPKIMount, mount.Type)
		}
	} else if out, err := store.authenticated(ctx, token, nil, "secrets", "enable", "-path="+defaultServerPKIMount, "-max-lease-ttl=87600h", "pki"); err != nil {
		return "", false, fmt.Errorf("enable server PKI mount: %w%s", err, commandOutputSuffix(out))
	}

	dnsNames, ips := serverPKINames(cfg)
	current, err := store.readData(ctx, token, defaultServerPKIMount+"/issuer/default")
	if err != nil && !strings.Contains(err.Error(), "no default issuer") {
		return "", false, err
	}
	if pemText, _ := current["certificate"].(string); pemText != "" {
		if ca, parseErr := parseCertificatePEM([]byte(pemText)); parseErr == nil &&
			serverCACovers(ca, dnsNames) && time.Until(ca.NotAfter) > serverCARenewBefore {
			caPEM = strings.TrimSpace(pemText) + "\n"
		}
	}
	if caPEM == "" {
		caPEM, err = generateServerCA(ctx, store, token, dnsNames)
		if err != nil {
			return "", false, err
		}
		replaced = true
	}

	var cidrs []string
	for _, ip := range ips {
		cidrs = append(cidrs, hostCIDR(net.ParseIP(ip)).String())
	}
	rolePath := defaultServerPKIMount + "/roles/" + defaultServerPKIRole
	wantRole := serverPKIRole(dnsNames, cidrs)
	haveRole, err := store.readData(ctx, token, rolePath)
	if err != nil {
		return "", false, err
	}
	if !serverPKIRoleMatches(haveRole, wantRole) {
		if err := store.writeJSON(ctx, token, rolePath, wantRole); err != nil {
			return "", false, err
		}
	}
	return caPEM, replaced, nil
}

// generateServerCA has OpenBao create a new name-constrained ECDSA P-256
// root with a non-exportable key and makes it the mount's default issuer.
func generateServerCA(ctx context.Context, store openBaoExec, token string, dnsNames []string) (string, error) {
	if len(dnsNames) == 0 {
		return "", errors.New("server CA needs at least one DNS name")
	}
	body, err := json.Marshal(map[string]any{
		"common_name":           "Oberth server CA",
		"issuer_name":           "server-ca-" + time.Now().UTC().Format("20060102t150405"),
		"key_type":              "ec",
		"key_bits":              256,
		"ttl":                   fmt.Sprintf("%dh", int(serverCAValidity.Hours())),
		"max_path_length":       0,
		"permitted_dns_domains": dnsNames,
		"exclude_cn_from_sans":  true,
	})
	if err != nil {
		return "", err
	}
	out, err := store.authenticated(ctx, token, body, "write", "-format=json", defaultServerPKIMount+"/root/generate/internal", "-")
	if err != nil {
		return "", fmt.Errorf("generate server CA: %w%s", err, commandOutputSuffix(out))
	}
	var response struct {
		Data struct {
			Certificate string `json:"certificate"`
			IssuerID    string `json:"issuer_id"`
		} `json:"data"`
	}
	if err := parseBaoJSON(out, &response); err != nil || response.Data.Certificate == "" || response.Data.IssuerID == "" {
		return "", fmt.Errorf("generate server CA: unexpected response%s", commandOutputSuffix(out))
	}
	if err := store.writeJSON(ctx, token, defaultServerPKIMount+"/config/issuers", map[string]any{
		"default": response.Data.IssuerID,
	}); err != nil {
		return "", err
	}
	return strings.TrimSpace(response.Data.Certificate) + "\n", nil
}

// serverCACovers reports whether the CA permits every requested DNS name
// as an exact permitted domain (a permitted domain also allows subdomains,
// so an exact match is the tightest test that still covers the name).
func serverCACovers(ca *x509.Certificate, dnsNames []string) bool {
	if !ca.IsCA || !ca.PermittedDNSDomainsCritical {
		return false
	}
	// IP constraints make the chain unusable for BoringSSL clients; replace
	// a CA that has them (one issued by an earlier build).
	if len(ca.PermittedIPRanges) > 0 || len(ca.ExcludedIPRanges) > 0 {
		return false
	}
	for _, name := range dnsNames {
		if !slices.Contains(ca.PermittedDNSDomains, name) {
			return false
		}
	}
	return true
}

func hostCIDR(ip net.IP) *net.IPNet {
	if ip4 := ip.To4(); ip4 != nil {
		return &net.IPNet{IP: ip4, Mask: net.CIDRMask(32, 32)}
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}
}

func parseCertificatePEM(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// serverPKIRole issues only the server's exact names and IPs, as ECDSA P-256
// leaves for TLS server use, unstored, for at most defaultServerLeafTTL.
func serverPKIRole(dnsNames, cidrs []string) map[string]any {
	return map[string]any{
		"issuer_ref":                  "default",
		"allowed_domains":             dnsNames,
		"allow_bare_domains":          true,
		"allow_subdomains":            false,
		"allow_glob_domains":          false,
		"allow_wildcard_certificates": false,
		"allow_any_name":              false,
		"enforce_hostnames":           true,
		"allow_ip_sans":               len(cidrs) > 0,
		"allowed_ip_sans_cidr":        cidrs,
		"key_type":                    "ec",
		"key_bits":                    256,
		"key_usage":                   []string{"DigitalSignature"},
		"server_flag":                 true,
		"client_flag":                 false,
		"ttl":                         defaultServerLeafTTL,
		"max_ttl":                     defaultServerLeafTTL,
		"no_store":                    true,
		"require_cn":                  true,
	}
}

// serverPKIRoleMatches compares a stored role with the wanted one, so a
// rerun on a configured store writes nothing. OpenBao reports TTLs in
// seconds and lists in any order.
func serverPKIRoleMatches(have, want map[string]any) bool {
	if have == nil {
		return false
	}
	for key, wantValue := range want {
		haveValue := have[key]
		switch w := wantValue.(type) {
		case []string:
			if !sameStringSet(anyStrings(haveValue), w) {
				return false
			}
		case string:
			if key == "ttl" || key == "max_ttl" {
				duration, err := time.ParseDuration(w)
				if err != nil || scalarString(haveValue) != fmt.Sprint(int64(duration.Seconds())) {
					return false
				}
				continue
			}
			if scalarString(haveValue) != w {
				return false
			}
		default:
			if scalarString(haveValue) != scalarString(w) {
				return false
			}
		}
	}
	return true
}

// scalarString renders a JSON-decoded scalar the way it was written: decoded
// numbers are float64, which fmt would print as 2.592e+06.
func scalarString(value any) string {
	if number, ok := value.(float64); ok {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return fmt.Sprint(value)
}

func anyStrings(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sortedA, sortedB := slices.Clone(a), slices.Clone(b)
	slices.Sort(sortedA)
	slices.Sort(sortedB)
	return slices.Equal(sortedA, sortedB)
}
