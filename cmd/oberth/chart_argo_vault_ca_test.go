package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The chart is the only thing that puts the OpenBao trust anchor in front of
// the server, and the server is the only thing that can put it in front of a
// release-tier pipeline Pod. A chart that renders the value but never mounts it
// produces a deployment whose release steps fail TLS verification with no
// indication that a chart value was ignored.
func TestChartDeliversTheArgoVaultTrustAnchorToTheServer(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	// A syntactically real certificate: the chart only moves bytes, but a
	// placeholder would let a template that mangles PEM indentation pass.
	const anchor = `-----BEGIN CERTIFICATE-----
MIIBIDCBxqADAgECAgEBMAoGCCqGSM49BAMCMBIxEDAOBgNVBAMTB3Rlc3QtY2Ew
-----END CERTIFICATE-----`

	rendered, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+digest,
		"--set", "argo.vault.address=https://openbao.openbao.svc:8200",
		"--set", "argo.vault.credentialedRole=oberth-argo-credentialed",
		"--set-string", "argo.vault.caCert="+anchor,
	).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	output := string(rendered)
	for _, want := range []string{
		"--argo-vault-ca-cert=/etc/oberth/argo-vault-ca/ca.crt",
		"name: oberth-argo-vault-ca",
		"mountPath: /etc/oberth/argo-vault-ca",
		"checksum/argo-vault-ca:",
		"-----BEGIN CERTIFICATE-----",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("rendered chart is missing %q", want)
		}
	}

	// Without the value there is no ConfigMap, no mount and no flag: a
	// publicly trusted store needs no anchor, and an empty one would render a
	// flag pointing at a file that does not exist. The address has to be a
	// public name for an empty anchor to carry that meaning at all -- see
	// TestChartRefusesAnInternalArgoVaultAddressWithoutAnAnchor.
	bare, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+digest,
		"--set", "argo.vault.address=https://bao.example.com:8200",
		"--set", "argo.vault.credentialedRole=oberth-argo-credentialed",
	).Output()
	if err != nil {
		t.Fatalf("render chart without an anchor: %v", err)
	}
	if strings.Contains(string(bare), "argo-vault-ca") {
		t.Error("the chart renders anchor plumbing for a deployment that declared none")
	}
}

// The deployment that failed the v0.12.4 release rendered cleanly: OpenBao at
// openbao.openbao.svc served a certificate from the installer's own private CA,
// argo.vault.address pointed release pipelines at it, and argo.vault.caCert was
// never set -- so no ConfigMap, no --argo-vault-ca-cert and no VAULT_CACERT
// existed, and every credentialed step died on "certificate signed by unknown
// authority" after the entire build had already passed.
//
// No publicly trusted CA may issue for a .svc name, so an empty anchor there
// cannot mean "the store is publicly trusted". It can only mean the release
// tier has no way to verify the store it is being sent to.
func TestChartRefusesAnInternalArgoVaultAddressWithoutAnAnchor(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	for name, address := range map[string]string{
		"service":       "https://openbao.openbao.svc:8200",
		"service FQDN":  "https://openbao.openbao.svc.cluster.local:8200",
		"single label":  "https://openbao:8200",
		"cluster IP":    "https://10.43.150.79:8200",
		"loopback":      "https://127.0.0.1:8200",
		"internal zone": "https://bao.corp.internal:8200",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
				"--set", "image.ref=example.invalid/oberth@"+digest,
				"--set", "argo.vault.address="+address,
				"--set", "argo.vault.credentialedRole=oberth-argo-credentialed",
			).CombinedOutput()
			if err == nil {
				t.Fatalf("chart rendered a release tier that can never verify %s", address)
			}
			if !strings.Contains(string(output), "no publicly trusted CA may issue for") {
				t.Fatalf("render error does not name the cause:\n%s", output)
			}
		})
	}
}

// The bundle reaches every release-tier Pod, so a private key in it is a
// serving identity handed to pipelines. The chart refuses it at render time,
// where the value is being typed, as well as the server at startup.
func TestChartRefusesAnArgoVaultBundleCarryingAPrivateKey(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const bundle = `-----BEGIN CERTIFICATE-----
MIIBIDCBxqADAgECAgEBMAoGCCqGSM49BAMCMBIxEDAOBgNVBAMTB3Rlc3QtY2Ew
-----END CERTIFICATE-----
-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIB9m0m0m0m0m0m0m0m0m0m0m0m0m0m0m0m0m0m0m0m0moAoGCCqGSM49
-----END EC PRIVATE KEY-----`

	output, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+digest,
		"--set", "argo.vault.address=https://openbao.openbao.svc:8200",
		"--set", "argo.vault.credentialedRole=oberth-argo-credentialed",
		"--set-string", "argo.vault.caCert="+bundle,
	).CombinedOutput()
	if err == nil || !strings.Contains(string(output), "must hold certificates only") {
		t.Fatalf("render error = %v\n%s", err, output)
	}

	// An anchor with no address to verify pins trust for a store no pipeline is
	// told to reach, which is a value that silently does nothing.
	orphaned, err := exec.Command("helm", "template", "oberth", "../../charts/oberth",
		"--set", "image.ref=example.invalid/oberth@"+digest,
		"--set-string", "argo.vault.caCert=-----BEGIN CERTIFICATE-----\naGk=\n-----END CERTIFICATE-----",
	).CombinedOutput()
	if err == nil || !strings.Contains(string(orphaned), "no pipeline is told to reach") {
		t.Fatalf("render error = %v\n%s", err, orphaned)
	}
}
