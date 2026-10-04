package installer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//go:embed all:rekorchart
var rekorChart embed.FS

// configureRekorBao runs while the installer still holds its administrative
// token. Only the public signing key leaves this provisioning phase.
func configureRekorBao(ctx context.Context, cfg Config, deps Deps, store openBaoExec, token string) (string, error) {
	ns := cfg.RekorNamespace
	if ns == "" {
		ns = DefaultRekorNamespace
	}
	// Adoption needs an explicit migration of the old log identity and database.
	// Never silently replace an existing signing key or password.
	for _, name := range []string{"rekor-signer", "trillian-mysql"} {
		_, err := deps.KubeClient.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return "", fmt.Errorf("rekor namespace %s contains legacy credential Secret %s; migrate and verify the existing log before upgrading", ns, name)
		}
		if !apierrors.IsNotFound(err) {
			return "", err
		}
	}
	keyPath := defaultTransitMount + "/keys/rekor-" + ns
	key, err := store.readData(ctx, token, keyPath)
	if err != nil {
		return "", err
	}
	if key == nil {
		if _, exists := findHelmRelease(ctx, deps, "rekor", ns); exists {
			return "", errors.New("existing Rekor release has no Transit key; refusing to replace its identity")
		}
		if err := store.writeJSON(ctx, token, keyPath, map[string]any{"type": "ecdsa-p256", "exportable": false, "allow_plaintext_backup": false}); err != nil {
			return "", err
		}
		key, err = store.readData(ctx, token, keyPath)
		if err != nil {
			return "", err
		}
	}
	pub, err := rekorTransitPublicKey(key)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"rekor-db", "rekor-db-root"} {
		path := "identities/" + ns + "/" + name
		existing, err := store.readData(ctx, token, defaultKVPrefix+"/data/"+path)
		if err != nil {
			return "", err
		}
		if existing != nil {
			data, _ := existing["data"].(map[string]any)
			password, _ := data["password"].(string)
			decoded, err := hex.DecodeString(password)
			if err != nil || len(decoded) != 32 {
				return "", fmt.Errorf("invalid existing Rekor database credential %s; refusing to replace it", name)
			}
			continue
		}
		metadata, err := store.readData(ctx, token, defaultKVPrefix+"/metadata/"+path)
		if err != nil {
			return "", err
		}
		if metadata != nil {
			return "", fmt.Errorf("rekor database credential %s was deleted; restore it before proceeding", name)
		}
		if _, exists := findHelmRelease(ctx, deps, "rekor", ns); exists {
			return "", errors.New("existing Rekor release has missing database credentials; refusing to generate replacements")
		}
		password := make([]byte, 32)
		if _, err := rand.Read(password); err != nil {
			return "", err
		}
		if err := store.writeJSON(ctx, token, defaultKVPrefix+"/data/"+path, map[string]any{"options": map[string]any{"cas": 0}, "data": map[string]any{"password": hex.EncodeToString(password)}}); err != nil {
			return "", err
		}
	}
	for _, component := range []string{"mysql", "db", "rekor"} {
		role := "rekor-" + ns + "-" + component
		policy := "path \"auth/token/renew-self\" { capabilities = [\"update\"] }\npath \"auth/token/lookup-self\" { capabilities = [\"read\"] }\n" + fmt.Sprintf("path %q { capabilities = [\"read\"] }\n", defaultKVPrefix+"/data/identities/"+ns+"/rekor-db")
		if component == "mysql" {
			policy += fmt.Sprintf("path %q { capabilities = [\"read\"] }\n", defaultKVPrefix+"/data/identities/"+ns+"/rekor-db-root")
		}
		if component == "rekor" {
			policy += fmt.Sprintf("path %q { capabilities = [\"read\"] }\npath %q { capabilities = [\"update\"] }\n", keyPath, defaultTransitMount+"/sign/rekor-"+ns+"/sha2-256")
		}
		if err := store.policyWrite(ctx, token, role, policy); err != nil {
			return "", err
		}
		rolePath := "auth/" + defaultAuthMount + "/role/" + role
		existing, err := store.readData(ctx, token, rolePath)
		if err != nil {
			return "", err
		}
		if existing != nil && (!exactSingletonString(existing["bound_service_account_names"], "rekor-bao-"+component) || !exactSingletonString(existing["bound_service_account_namespaces"], ns) || !exactSingletonString(existing["token_policies"], role)) {
			return "", fmt.Errorf("rekor role %s has incompatible bindings; refusing to overwrite", role)
		}
		if err := store.writeJSON(ctx, token, rolePath, map[string]any{"bound_service_account_names": "rekor-bao-" + component, "bound_service_account_namespaces": ns, "token_policies": role, "token_no_default_policy": true, "token_ttl": "10m", "token_max_ttl": "15m"}); err != nil {
			return "", err
		}
	}
	return pub, nil
}

func rekorTransitPublicKey(key map[string]any) (string, error) {
	if key["type"] != "ecdsa-p256" || key["exportable"] != false || key["allow_plaintext_backup"] != false || key["derived"] != false || key["latest_version"] != float64(1) {
		return "", errors.New("rekor Transit key must be a non-exportable, non-derived, unrotated ECDSA P-256 key")
	}
	versions, _ := key["keys"].(map[string]any)
	first, _ := versions["1"].(map[string]any)
	pub, _ := first["public_key"].(string)
	block, _ := pem.Decode([]byte(pub))
	if block == nil {
		return "", errors.New("rekor Transit key has no public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", errors.New("invalid Rekor Transit public key")
	}
	ec, ok := parsed.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return "", errors.New("rekor Transit public key must be ECDSA P-256")
	}
	return pub, nil
}

func extractRekorChart() (string, error) {
	dir, err := os.MkdirTemp("", "oberth-rekor-chart-")
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(rekorChart, "rekorchart", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(dir, path)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		body, err := rekorChart.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0600)
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// rekorConnectionValues contains public trust and routing data only.
func rekorConnectionValues(cfg Config) string {
	data, _ := json.Marshal(map[string]any{"global": map[string]any{"bao": map[string]any{"address": cfg.rekorBaoAddress, "caCert": cfg.rekorBaoCA}}})
	return string(data)
}
