package vmpilotfixture

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func plan() vmrunner.PilotPlan {
	return vmrunner.PilotPlan{Version: 1, Profile: vmrunner.BeaconTransportProfile,
		Spec: vmrunner.VMRunSpec{RunID: "fixture-run", Repo: "beacon", CandidateSHA: strings.Repeat("a", 40),
			SuiteRevision: strings.Repeat("b", 40), GuestImageRef: "registry.example/guest@sha256:" + strings.Repeat("c", 64),
			Resources: vmrunner.VMResources{CPUCores: 1, MemoryMiB: 512}, Deadline: 5 * time.Minute},
		ConductorImageRef: "registry.example/conductor@sha256:" + strings.Repeat("d", 64),
		KernelDigest:      "sha256:" + strings.Repeat("1", 64), InitramfsDigest: "sha256:" + strings.Repeat("2", 64),
		GuestHelperDigest: "sha256:" + strings.Repeat("3", 64), ArtifactDigest: "sha256:" + strings.Repeat("4", 64), ArtifactBytes: 100,
		GuestNamespace: "pilot-guest", ConductorNamespace: "pilot-conductor", ServerNamespace: "oberth"}
}

func newFixture(t *testing.T) *Fixture {
	t.Helper()
	f, err := New(plan())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func initial(t *testing.T, f *Fixture) vmrunner.PilotEndpoint {
	t.Helper()
	ep, err := f.BindEndpoint(vmrunner.InitialVMResource, "vmi-one", "launcher-one", "10.0.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func conductor() vmrunner.ConductorAttempt {
	return vmrunner.ConductorAttempt{AttemptID: "attempt-one", JobUID: "job-one", PodUID: "pod-one", Container: "conductor",
		ContainerID: "containerd://one", ImageDigest: "sha256:" + strings.Repeat("d", 64),
		SpecIdentity: strings.Repeat("e", 64), StartedAt: time.Now().UTC()}
}

func allZero(value []byte) bool { return bytes.Equal(value, make([]byte, len(value))) }

func TestFreshSignedLeavesAndScopedGuestDelivery(t *testing.T) {
	f := newFixture(t)
	first := initial(t, f)
	second, err := f.BindEndpoint(vmrunner.RestartVMResource, "vmi-two", "launcher-two", "10.0.0.2:443")
	if err != nil {
		t.Fatal(err)
	}
	if first.CertificateSHA256 == second.CertificateSHA256 || first.FixtureGeneration != second.FixtureGeneration || len(f.caKey) != 0 {
		t.Fatal("restart did not rotate leaf or discard exhausted CA signing key")
	}
	pool := x509.NewCertPool()
	pool.AddCert(f.ca)
	if f.ca.CheckSignatureFrom(f.ca) != nil || !f.ca.IsCA {
		t.Fatal("CA is not independently self signed")
	}
	var publicKeys []ed25519.PublicKey
	for _, endpoint := range []vmrunner.PilotEndpoint{first, second} {
		var borrowed GuestMaterial
		err := f.WithGuestMaterial(endpoint, func(material GuestMaterial) error {
			borrowed = material
			pair, err := tls.X509KeyPair(material.CertificatePEM, material.PrivateKeyPEM)
			if err != nil {
				t.Fatal("leaf key does not match certificate")
			}
			cert := pair.Leaf
			if cert == nil {
				t.Fatal("certificate not parsed")
			}
			if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "beacon.fixture", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil || cert.IsCA {
				t.Fatal("invalid server-only leaf chain")
			}
			if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: "wrong.fixture"}); err == nil {
				t.Fatal("wrong identity accepted")
			}
			if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
				t.Fatal("leaf can authenticate as client")
			}
			digest := sha256.Sum256(cert.Raw)
			if hex.EncodeToString(digest[:]) != endpoint.CertificateSHA256 {
				t.Fatal("leaf pin mismatch")
			}
			publicKeys = append(publicKeys, cert.PublicKey.(ed25519.PublicKey))
			for i, tenant := range material.Tenants {
				if tenant.ID != f.tenants[i].ID || !bytes.Equal(tenant.Key, f.tenants[i].Key) || len(tenant.Key) != 32 {
					t.Fatal("tenant capability differs from run")
				}
			}
			if _, err := json.Marshal(material); err == nil {
				t.Fatal("general purpose guest serialization allowed")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !allZero(borrowed.PrivateKeyPEM) || !allZero(borrowed.CertificatePEM) || !allZero(borrowed.Tenants[0].Key) || !allZero(borrowed.Tenants[1].Key) {
			t.Fatal("borrowed buffers survived callback")
		}
		if err := f.WithGuestMaterial(endpoint, func(GuestMaterial) error { t.Fatal("duplicate delivery invoked"); return nil }); !errors.Is(err, ErrBinding) {
			t.Fatal("duplicate guest delivery accepted")
		}
	}
	if bytes.Equal(publicKeys[0], publicKeys[1]) || bytes.Equal(publicKeys[0], f.ca.PublicKey.(ed25519.PublicKey)) {
		t.Fatal("leaf keys are not independent")
	}
}

// This independent wire shape follows the E2E conductor's field ordering. It
// intentionally does not use the production bootstrap type for re-encoding.
type wireTenant struct {
	ID  string `json:"id"`
	Key []byte `json:"key"`
}
type wireBootstrap struct {
	Version       int                    `json:"version"`
	Type          string                 `json:"type"`
	Execution     string                 `json:"execution"`
	Attempt       string                 `json:"attempt"`
	Inventory     string                 `json:"inventory"`
	SuiteRevision string                 `json:"suite_revision"`
	TimeoutMillis int64                  `json:"timeout_millis"`
	KeyScope      string                 `json:"key_scope"`
	RootCA        []byte                 `json:"root_ca"`
	Tenants       [2]wireTenant          `json:"tenants"`
	Endpoint      vmrunner.PilotEndpoint `json:"endpoint"`
}

func TestCanonicalBootstrapContainsOnlyConductorCapabilities(t *testing.T) {
	f := newFixture(t)
	ep, attempt := initial(t, f), conductor()
	var borrowed []byte
	err := f.WithBootstrap(attempt, ep, func(frame []byte) error {
		borrowed = frame
		if len(frame) > 32<<10 || frame[len(frame)-1] != '\n' || bytes.Count(frame, []byte{'\n'}) != 1 {
			t.Fatal("not one bounded canonical frame")
		}
		var value wireBootstrap
		if json.Unmarshal(frame, &value) != nil {
			t.Fatal("bootstrap is not valid conductor JSON")
		}
		defer func() {
			for i := range value.Tenants {
				clear(value.Tenants[i].Key)
			}
		}()
		canonical, err := json.Marshal(value)
		defer clear(canonical)
		if err != nil || !bytes.Equal(frame[:len(frame)-1], canonical) {
			t.Fatal("schema/order differs from conductor contract")
		}
		if value.Version != 1 || value.Type != "bootstrap" || value.KeyScope != "per-run" || value.TimeoutMillis != plan().Spec.Deadline.Milliseconds() || value.Endpoint != ep ||
			value.Execution != vmrunner.PilotIdentity(plan()) || value.Attempt != vmrunner.ConductorAttemptIdentity(attempt) || value.SuiteRevision != plan().Spec.SuiteRevision {
			t.Fatal("bootstrap identity mismatch")
		}
		inventory, _ := json.Marshal(struct {
			Profile, Suite string
			Cases          []string
		}{
			"beacon-transport-amd64-v2", plan().Spec.SuiteRevision,
			[]string{"authenticated-relay", "wrong-tenant-key", "invalid-token", "replayed-token", "missing-v2-tenant", "near-match-tenant", "near-match-domain", "restart-fresh-relay"}})
		digest := sha256.Sum256(inventory)
		if value.Inventory != hex.EncodeToString(digest[:]) {
			t.Fatal("independent conductor inventory mismatch")
		}
		block, rest := pem.Decode(value.RootCA)
		if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 || !bytes.Equal(block.Bytes, f.ca.Raw) {
			t.Fatal("bootstrap root is not the sole public CA certificate")
		}
		if bytes.Contains(frame, []byte("PRIVATE KEY")) || bytes.Equal(value.Tenants[0].Key, value.Tenants[1].Key) || value.Tenants[0].ID == value.Tenants[1].ID {
			t.Fatal("bootstrap leaks private key or repeats tenant capability")
		}
		return nil
	})
	if err != nil || !allZero(borrowed) {
		t.Fatal("bootstrap delivery or clearing failed")
	}
	if err := f.WithBootstrap(attempt, ep, func([]byte) error { t.Fatal("second bootstrap emitted"); return nil }); !errors.Is(err, ErrBinding) {
		t.Fatal("repeated bootstrap accepted")
	}
}

func TestEndpointAndConductorMutationsAreRejectedBeforeDelivery(t *testing.T) {
	for _, attack := range []string{"wrong-vmi", "wrong-launcher", "wrong-address", "wrong-pin", "wrong-generation", "wrong-slot", "wrong-image", "wrong-container", "old-attempt", "future-attempt"} {
		t.Run(attack, func(t *testing.T) {
			f := newFixture(t)
			ep, attempt := initial(t, f), conductor()
			switch attack {
			case "wrong-vmi":
				ep.VMIUID = "replacement"
			case "wrong-launcher":
				ep.LauncherUID = "replacement"
			case "wrong-address":
				ep.Address = "10.0.0.9:443"
			case "wrong-pin":
				ep.CertificateSHA256 = strings.Repeat("f", 64)
			case "wrong-generation":
				ep.FixtureGeneration = strings.Repeat("f", 64)
			case "wrong-slot":
				ep.Attempt = vmrunner.RestartVMResource
			case "wrong-image":
				attempt.ImageDigest = "sha256:" + strings.Repeat("f", 64)
			case "wrong-container":
				attempt.Container = "guest"
			case "old-attempt":
				attempt.StartedAt = f.created.Add(-time.Second)
			case "future-attempt":
				attempt.StartedAt = time.Now().Add(time.Minute)
			}
			if err := f.WithBootstrap(attempt, ep, func([]byte) error { t.Fatal("invalid binding delivered keys"); return nil }); !errors.Is(err, ErrBinding) {
				t.Fatal("invalid binding accepted")
			}
		})
	}
	f := newFixture(t)
	if _, err := f.BindEndpoint(vmrunner.RestartVMResource, "two", "two", "10.0.0.2:443"); !errors.Is(err, ErrBinding) {
		t.Fatal("restart before initial accepted")
	}
	ep := initial(t, f)
	for _, args := range [][4]string{
		{vmrunner.InitialVMResource, "new", "new", "10.0.0.9:443"},
		{vmrunner.RestartVMResource, ep.VMIUID, "new", "10.0.0.9:443"},
		{vmrunner.RestartVMResource, "new", ep.LauncherUID, "10.0.0.9:443"},
		{vmrunner.RestartVMResource, "new", "new", "10.0.0.9:444"},
		{"beacon-2", "new", "new", "10.0.0.9:443"},
	} {
		if _, err := f.BindEndpoint(args[0], args[1], args[2], args[3]); !errors.Is(err, ErrBinding) {
			t.Fatal("endpoint rebinding accepted")
		}
	}
}

func TestFailedDeliveryConsumesAndClearsWithoutEchoingError(t *testing.T) {
	f := newFixture(t)
	ep, attempt := initial(t, f), conductor()
	var borrowed []byte
	err := f.WithBootstrap(attempt, ep, func(frame []byte) error { borrowed = frame; return errors.New("sensitive adapter detail") })
	if !errors.Is(err, ErrDelivery) || strings.Contains(err.Error(), "sensitive") || !allZero(borrowed) || !f.bootstrapped {
		t.Fatal("failed bootstrap not consumed/cleared/redacted")
	}
	var guest GuestMaterial
	err = f.WithGuestMaterial(ep, func(m GuestMaterial) error { guest = m; return errors.New("sensitive adapter detail") })
	if !errors.Is(err, ErrDelivery) || !allZero(guest.PrivateKeyPEM) || !allZero(f.leaves[0].key) || !f.leaves[0].delivered {
		t.Fatal("failed guest delivery not consumed/cleared")
	}
	if got := fmt.Sprintf("%v %#v %v %#v", f, f, guest, guest); strings.Contains(got, "PRIVATE KEY") || !strings.Contains(got, "ephemeral Beacon fixture") {
		t.Fatal("unsafe formatted capability")
	}
}

func TestCloseWaitsForBorrowThenClearsOwnedBuffers(t *testing.T) {
	f := newFixture(t)
	ep := initial(t, f)
	caKey, tenantKey, leafKey := f.caKey, f.tenants[0].Key, f.leaves[0].key
	entered, release, closeStarted, closeDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := f.WithGuestMaterial(ep, func(GuestMaterial) error { close(entered); <-release; return nil }); err != nil {
			t.Error(err)
		}
	})
	<-entered
	wg.Go(func() { close(closeStarted); f.Close(); close(closeDone) })
	<-closeStarted
	select {
	case <-closeDone:
		t.Fatal("Close returned with active borrowed material")
	default:
	}
	close(release)
	wg.Wait()
	if !allZero(caKey) || !allZero(tenantKey) || !allZero(leafKey) {
		t.Fatal("Close left owned capabilities")
	}
	if _, err := f.BindEndpoint(vmrunner.RestartVMResource, "two", "two", "10.0.0.2:443"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("closed fixture available")
	}
	if err := f.WithBootstrap(conductor(), ep, func([]byte) error { t.Fatal("closed delivery"); return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatal("closed bootstrap available")
	}
}

func TestSecondPrecisionHostStartIsAccepted(t *testing.T) {
	f := newFixture(t)
	ep, attempt := initial(t, f), conductor()
	attempt.StartedAt = f.created.Truncate(time.Second)
	if err := f.WithBootstrap(attempt, ep, func([]byte) error { return nil }); err != nil {
		t.Fatal("second-precision host timestamp rejected")
	}
}

func TestPrecisePrecreationStartInSameSecondIsRejected(t *testing.T) {
	f := newFixture(t)
	base := time.Now().Add(-time.Second).Truncate(time.Second)
	f.created = base.Add(800 * time.Millisecond)
	ep, attempt := initial(t, f), conductor()
	attempt.StartedAt = base.Add(400 * time.Millisecond)
	called := false
	err := f.WithBootstrap(attempt, ep, func([]byte) error { called = true; return nil })
	if !errors.Is(err, ErrBinding) || called || f.bootstrapped {
		t.Fatal("precise precreation attempt received fixture capabilities")
	}
}

func TestPlansExpiryAndIndependentRuns(t *testing.T) {
	bad := plan()
	bad.Spec.Deadline = time.Second
	if _, err := New(bad); !errors.Is(err, ErrBinding) {
		t.Fatal("unsupported timeout accepted")
	}
	a, b := newFixture(t), newFixture(t)
	if a.generation == b.generation || bytes.Equal(a.ca.Raw, b.ca.Raw) || bytes.Equal(a.tenants[0].Key, b.tenants[0].Key) {
		t.Fatal("run capabilities reused")
	}
	a.expires = time.Now().Add(-time.Second)
	if _, err := a.BindEndpoint(vmrunner.InitialVMResource, "one", "one", "10.0.0.1:443"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("expired fixture available")
	}
}
