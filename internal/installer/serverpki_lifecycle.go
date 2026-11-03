package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func unionNames(a, b []string) []string {
	out := append(slices.Clone(a), b...)
	slices.Sort(out)
	return slices.Compact(out)
}

func removeNames(names, removed []string) []string {
	return slices.DeleteFunc(slices.Clone(names), func(name string) bool { return slices.Contains(removed, name) })
}

func mergeTLSNames(cfg *Config, live liveHelmValuesSubset) {
	// Canonical IP spelling keeps explicit IPv6 removal consistent in Helm
	// values, the role's host CIDRs, and the certificate's parsed IP SANs.
	for i, address := range live.TLS.ExtraIPs {
		if ip := net.ParseIP(address); ip != nil {
			live.TLS.ExtraIPs[i] = ip.String()
		}
	}
	cfg.TLSExtraDNSNames = removeNames(unionNames(cfg.TLSExtraDNSNames, live.TLS.ExtraDNSNames), cfg.TLSRemoveDNSNames)
	cfg.TLSExtraIPs = removeNames(unionNames(cfg.TLSExtraIPs, live.TLS.ExtraIPs), cfg.TLSRemoveIPs)
}

func validateServerTLSFlags(cfg *Config) error {
	for _, field := range []struct {
		flag  string
		names *[]string
	}{{"--tls-extra-ip", &cfg.TLSExtraIPs}, {"--tls-remove-ip", &cfg.TLSRemoveIPs}} {
		*field.names = slices.Clone(*field.names)
		for i, address := range *field.names {
			ip := net.ParseIP(address)
			if ip == nil {
				return fmt.Errorf("%s %q is not an IP address", field.flag, address)
			}
			(*field.names)[i] = ip.String()
		}
	}
	base, _ := serverPKINames(Config{Namespace: cfg.Namespace})
	for _, name := range cfg.TLSRemoveDNSNames {
		if slices.Contains(base, name) {
			return fmt.Errorf("cannot remove required in-cluster TLS name %s", name)
		}
		if slices.Contains(cfg.TLSExtraDNSNames, name) {
			return fmt.Errorf("TLS DNS name %s cannot be both added and removed", name)
		}
	}
	for _, ip := range cfg.TLSRemoveIPs {
		if slices.Contains(cfg.TLSExtraIPs, ip) {
			return fmt.Errorf("TLS IP %s cannot be both added and removed", ip)
		}
	}
	return nil
}

// Keep the runtime request's SANs as well as the OpenBao role's permissions.
func preserveServerTLSNames(ctx context.Context, cfg *Config, deps Deps) error {
	if deps.KubeClient == nil {
		return nil
	}
	_, err := deps.KubeClient.AppsV1().Deployments(cfg.Namespace).Get(ctx, "oberth", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("locate existing server TLS configuration: %w", err)
	}
	live, err := readLiveHelmValuesSubset(ctx, deps, cfg.Namespace)
	if err != nil {
		return fmt.Errorf("read existing server TLS names: %w", err)
	}
	mergeTLSNames(cfg, live)
	cfg.serverPKI = cfg.serverPKI || live.TLS.PKI.Enabled
	if len(cfg.TLSRemoveDNSNames)+len(cfg.TLSRemoveIPs) > 0 && !cfg.serverPKI {
		return errors.New("TLS name removal requires server PKI")
	}
	return checkServerPKIRemovals(ctx, *cfg, deps)
}

func mergedServerPKINames(cfg Config, role map[string]any) ([]string, []string, error) {
	dns, ips := serverPKINames(cfg)
	base, _ := serverPKINames(Config{Namespace: cfg.Namespace})
	for _, name := range cfg.TLSRemoveDNSNames {
		if slices.Contains(base, name) {
			return nil, nil, fmt.Errorf("cannot remove required in-cluster TLS name %s", name)
		}
	}
	dns = removeNames(unionNames(dns, anyStrings(role["allowed_domains"])), cfg.TLSRemoveDNSNames)
	cidrs := anyStrings(role["allowed_ip_sans_cidr"])
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return nil, nil, fmt.Errorf("invalid server TLS IP address %q", ip)
		}
		cidrs = append(cidrs, hostCIDR(parsed).String())
	}
	for _, ip := range cfg.TLSRemoveIPs {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return nil, nil, fmt.Errorf("--tls-remove-ip %q is not an IP address", ip)
		}
		cidrs = removeNames(cidrs, []string{hostCIDR(parsed).String()})
	}
	return dns, unionNames(cidrs, nil), nil
}

// Check the public leaf before any write, including mount/issuer writes.
func checkServerPKIRemovals(ctx context.Context, cfg Config, deps Deps) error {
	if len(cfg.TLSRemoveDNSNames)+len(cfg.TLSRemoveIPs) == 0 {
		return nil
	}
	pem, err := readPodServerCert(ctx, cfg, deps)
	if err != nil {
		return fmt.Errorf("cannot check served SANs before TLS name removal: %w", err)
	}
	cert, err := parseCertificatePEM(pem)
	if err != nil {
		return fmt.Errorf("parse served certificate before TLS name removal: %w", err)
	}
	var missing []string
	for _, name := range cfg.TLSRemoveDNSNames {
		if slices.Contains(cert.DNSNames, name) {
			missing = append(missing, fmt.Sprintf("DNS %s (keep with --tls-extra-dns-name %s)", name, name))
		}
	}
	for _, address := range cfg.TLSRemoveIPs {
		ip := net.ParseIP(address)
		if ip == nil {
			return fmt.Errorf("--tls-remove-ip %q is not an IP address", address)
		}
		for _, served := range cert.IPAddresses {
			if ip.Equal(served) {
				missing = append(missing, fmt.Sprintf("IP %s (keep with --tls-extra-ip %s)", address, address))
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("refusing to remove SANs on the currently served certificate: %s", strings.Join(missing, "; "))
	}
	return nil
}

func missingPKINames(role map[string]any, dns, ips []string) []string {
	var missing []string
	for _, name := range dns {
		if !slices.Contains(anyStrings(role["allowed_domains"]), name) {
			missing = append(missing, fmt.Sprintf("DNS %s (--tls-extra-dns-name %s)", name, name))
		}
	}
	for _, address := range ips {
		allowed := false
		for _, cidr := range anyStrings(role["allowed_ip_sans_cidr"]) {
			_, network, err := net.ParseCIDR(cidr)
			if err == nil && network.Contains(net.ParseIP(address)) && role["allow_ip_sans"] == true {
				allowed = true
			}
		}
		if !allowed {
			missing = append(missing, fmt.Sprintf("IP %s (--tls-extra-ip %s)", address, address))
		}
	}
	return missing
}

func checkServerPKIRole(ctx context.Context, cfg Config, deps Deps, store openBaoExec, token string, planned bool) error {
	if err := checkServerPKIRemovals(ctx, cfg, deps); err != nil {
		return err
	}
	role, err := store.readData(ctx, token, defaultServerPKIMount+"/roles/"+defaultServerPKIRole)
	if err != nil {
		return err
	}
	if planned {
		dns, cidrs, err := mergedServerPKINames(cfg, role)
		if err != nil {
			return err
		}
		role = serverPKIRole(dns, cidrs)
	}
	dns, ips := serverPKINames(cfg)
	// Include the currently served SANs: an omitted CLI flag cannot hide drift.
	leafPEM, err := readPodServerCert(ctx, cfg, deps)
	if err == nil {
		leaf, parseErr := parseCertificatePEM(leafPEM)
		err = parseErr
		if err == nil {
			dns = unionNames(dns, leaf.DNSNames)
			for _, ip := range leaf.IPAddresses {
				ips = append(ips, ip.String())
			}
		}
	}
	if err != nil {
		if len(cfg.TLSRemoveDNSNames)+len(cfg.TLSRemoveIPs) > 0 {
			return fmt.Errorf("read served certificate for PKI preflight before TLS name removal: %w", err)
		}
		// Removal checks above require a readable leaf. With no removal,
		// allow repair of a server that cannot start with its current role.
		w := deps.Output
		if w == nil {
			w = io.Discard
		}
		_, _ = fmt.Fprintf(w, "WARNING: cannot read the served certificate for PKI preflight: %v; checking role permissions against expected TLS names only.\n", err)
	}
	if missing := missingPKINames(role, dns, unionNames(ips, nil)); len(missing) > 0 {
		return fmt.Errorf("server PKI role %s is missing names: %s; inspect: bao read %s/roles/%s; reconcile with oberth install --upgrade --install-secretstore and the listed flags", defaultServerPKIRole, strings.Join(missing, "; "), defaultServerPKIMount, defaultServerPKIRole)
	}
	return nil
}

func serverPKIAccess(ctx context.Context, cfg Config, deps Deps) (openBaoExec, string, error) {
	token := installerBaoToken(deps)
	if token == "" {
		return openBaoExec{}, "", errors.New("server PKI inspection requires BAO_TOKEN")
	}
	if deps.KubeClient == nil {
		return openBaoExec{}, "", errors.New("server PKI inspection requires a Kubernetes client")
	}
	ns := cfg.OpenBaoNamespace
	if ns == "" {
		ns = DefaultOpenBaoNamespace
	}
	pod, err := findOpenBaoPod(ctx, deps, ns)
	if err != nil {
		return openBaoExec{}, "", err
	}
	return newOpenBaoExec(deps, ns, pod), token, nil
}

// PreflightPKI reads the deployed role and public leaf without modifying either.
func PreflightPKI(ctx context.Context, cfg Config, deps Deps) error {
	if err := validateServerTLSFlags(&cfg); err != nil {
		return err
	}
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	live, err := readLiveHelmValuesSubset(ctx, deps, cfg.Namespace)
	if err != nil {
		return err
	}
	if !live.TLS.PKI.Enabled {
		return errors.New("server PKI is not enabled in the selected release")
	}
	mergeTLSNames(&cfg, live)
	store, token, err := serverPKIAccess(ctx, cfg, deps)
	if err != nil {
		return err
	}
	return checkServerPKIRole(ctx, cfg, deps, store, token, false)
}
