package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/clientcert"
)

func TestFirstAdminCrashSafeInstaller(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var firstCSR string
	deps := Deps{ContextName: "selected", ClientConfigRoot: func() (string, error) { return root, nil }}
	deps.RunCommand = func(_ context.Context, body []byte, name string, args ...string) ([]byte, error) {
		calls++
		path := filepath.Join(root, pendingInstallerName(Config{}, deps))
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("exec preceded durable owner-only key and CSR", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var pending pendingInstallerEnrollment
		if json.Unmarshal(raw, &pending) != nil || validateInstallerEnrollment(pending.Key, pending.CSR) != nil {
			t.Fatal("pending key/CSR invalid")
		}
		var request api.FirstAdminRequest
		if json.Unmarshal(body, &request) != nil || request.InstallerCSR != string(pending.CSR) || bytes.Contains(body, []byte("PRIVATE KEY")) {
			t.Fatal("exec does not use persisted public CSR")
		}
		if calls == 1 {
			firstCSR = request.InstallerCSR
			return nil, io.ErrUnexpectedEOF
		}
		if request.InstallerCSR != firstCSR {
			t.Fatal("retry replaced persisted key")
		}
		return json.Marshal(api.FirstAdminResult{InstallerCertificate: "certificate", ServerCA: "public CA"})
	}
	for attempt := 0; attempt < 2; attempt++ {
		enrollment, err := newInstallerEnrollment()
		if err != nil {
			t.Fatal(err)
		}
		_, err = createFirstAdmin(t.Context(), Config{enrollment: enrollment}, deps, []byte("SSH public key"), "owner@host")
		clear(enrollment.key)
		if (attempt == 0) != (err != nil) {
			t.Fatal("unexpected retry result", attempt, err)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	cfg := Config{}
	deps.ContextName = "other"
	if err := reusePendingInstallerEnrollment(&cfg, deps); err != nil || cfg.enrollment != nil {
		t.Fatal("pending key crossed cluster targets", err)
	}
}

func TestAdminClusterIdentitySecurity(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	authority, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	clear(authority["ca.key"])
	replacement, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	clear(replacement["ca.key"])
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "selected"
	config.Clusters["selected"] = &clientcmdapi.Cluster{Server: "https://selected.invalid", CertificateAuthorityData: authority["ca.crt"]}
	config.Contexts["selected"] = &clientcmdapi.Context{Cluster: "selected"}
	path := filepath.Join(root, "kubeconfig")
	t.Setenv("KUBECONFIG", path)
	write := func() {
		t.Helper()
		if err := clientcmd.WriteToFile(*config, path); err != nil {
			t.Fatal(err)
		}
	}
	write()
	dir, err := client.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := recordKubernetesTarget(Config{Namespace: "oberth", KubeconfigPath: path}, Deps{ContextName: "selected", ClientConfigRoot: func() (string, error) { return dir, nil }}); err != nil {
		t.Fatal(err)
	}
	t.Run("CAFileSymlink", func(t *testing.T) {
		cluster := config.Clusters["selected"]
		saved := cluster.DeepCopy()
		defer func() { config.Clusters["selected"] = saved; write() }()
		source := filepath.Join(root, "public-ca.pem")
		if err := os.WriteFile(source, authority["ca.crt"], 0600); err != nil {
			t.Fatal(err)
		}
		links := filepath.Join(root, "ca-links")
		if err := os.Mkdir(links, 0700); err != nil {
			t.Fatal(err)
		}
		selected := filepath.Join(links, "cluster-ca.pem")
		if err := os.Symlink(source, selected); err != nil {
			t.Fatal(err)
		}
		cluster.CertificateAuthorityData = nil
		cluster.CertificateAuthority = selected
		write()
		if _, _, _, err := loadKubeConfigForContext("selected", "", io.Discard); err != nil {
			t.Fatal("equivalent CA file was refused", err)
		}
		_, _, _, cleanup, err := pinnedKubernetesCommand("kubectl", []string{"get", "pods"}, os.Environ())
		_ = cleanup()
		if err != nil {
			t.Fatal("equivalent CA file refused for child command", err)
		}
	})
	for _, change := range []string{"server", "CA", "TLS name", "TLS verification"} {
		t.Run(change, func(t *testing.T) {
			cluster := config.Clusters["selected"]
			saved := cluster.DeepCopy()
			defer func() { config.Clusters["selected"] = saved; write() }()
			switch change {
			case "server":
				cluster.Server = "https://different.invalid"
			case "CA":
				cluster.CertificateAuthorityData = replacement["ca.crt"]
			case "TLS name":
				cluster.TLSServerName = "different.invalid"
			case "TLS verification":
				cluster.InsecureSkipTLSVerify = true
			}
			write()
			for _, name := range []string{"kubectl", "helm"} {
				for _, explicit := range []bool{false, true} {
					args := []string{"get", "pods"}
					if name == "helm" {
						args = []string{"list"}
					}
					if explicit {
						flag := "--context"
						if name == "helm" {
							flag = "--kube-context"
						}
						args = append([]string{flag, "selected"}, args...)
					}
					_, _, _, cleanup, err := pinnedKubernetesCommand(name, args, os.Environ())
					_ = cleanup()
					if err == nil {
						t.Fatal("changed cluster accepted", name, explicit)
					}
				}
			}
			if _, _, _, err := loadKubeConfigForContext("selected", "", io.Discard); err == nil {
				t.Fatal("client-go accepted changed cluster")
			}
		})
	}
	if _, _, _, err := loadKubeConfigForContext("selected", "", io.Discard); err != nil {
		t.Fatal("unchanged cluster refused", err)
	}
	record, err := RecordedKubernetesTarget()
	if err != nil || record.APIServer != "https://selected.invalid" || len(record.CASHA256) != 64 {
		t.Fatal("identity absent from record", err)
	}
}
