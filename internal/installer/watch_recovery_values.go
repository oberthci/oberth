package installer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/goproxy"
	"github.com/oberthci/oberth/internal/store"
)

// Only v2 can preserve the deployment's existing protocol and module namespace.
// The public plan binds the canonical bytes; ordinary v1 keeps its old whitelist.
func readWatchConfiguredValues(cfg Config) ([]byte, error) {
	raw, err := readWatchValues(cfg.ValuesFiles)
	if err == nil {
		return raw, nil
	}
	if cfg.Namespace == "" {
		cfg.Namespace = DefaultNamespace
	}
	if cfg.ChartVersion == "" {
		cfg.ChartVersion = cfg.BinaryVersion
	}
	p, planErr := readWatchRecoveryPlan(cfg)
	if planErr != nil || p == nil {
		return nil, err
	}
	raw, err = readWatchValuesMode(cfg.ValuesFiles, true)
	if err != nil || watchValuesDigest(raw) != p.ValuesSHA256 {
		return nil, errors.New("watch recovery public values differ from the approved plan")
	}
	return raw, nil
}

func watchValuesDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func mergeWatchRecoveryValues(merged, fields map[string]any, section string) error {
	refuse := func() error { return errors.New("watch recovery compatibility values are incomplete or unapproved") }
	if fields == nil {
		return refuse()
	}
	if section == "compatibility" {
		if len(fields) != 3 {
			return refuse()
		}
		schema, a := fields["schemaIdentity"].(string)
		audit, b := fields["auditDomain"].(string)
		witness, c := fields["witnessKeyInfo"].(string)
		if !a || !b || !c || (store.ProtocolConfig{SchemaIdentity: schema, AuditDomain: audit}).Validate() != nil || auditanchor.ValidateWitnessKeyInfo(witness) != nil {
			return refuse()
		}
	} else {
		proxy, ok := fields["goProxy"].(map[string]any)
		if !ok || len(fields) != 1 || len(proxy) != 4 {
			return refuse()
		}
		module, a := proxy["modulePrefix"].(string)
		repository, b := proxy["repositoryPrefix"].(string)
		upstream, c := proxy["upstream"].(string)
		organization, d := proxy["organization"].(string)
		if !a || !b || !c || !d || len(module) > 253 || (goproxy.NamespaceConfig{ModulePrefix: module, RepositoryPrefix: repository, Upstream: upstream, Organization: organization}).Validate() != nil {
			return refuse()
		}
	}
	for key, value := range fields {
		if previous, exists := merged[key]; exists {
			a, _ := json.Marshal(previous)
			b, _ := json.Marshal(value)
			if !bytes.Equal(a, b) {
				return errors.New("conflicting watch recovery public values files")
			}
		}
		merged[key] = value
	}
	return nil
}

// Freeze and preparation are independent reads. Only the sealed canonical input
// is handed to Helm; the administrator's mutable source paths are never reused.
func requireWatchRecoveryValues(cfg Config, p watchRecoveryPlan) error {
	if len(cfg.ValuesFiles) != 1 || cfg.watchValuesSHA256 != p.ValuesSHA256 {
		return errors.New("watch recovery values were not frozen from the approved plan")
	}
	f, err := os.Open(cfg.ValuesFiles[0]) // #nosec G304 -- private prepared sealed descriptor, bounded and hash-bound below.
	if err != nil {
		return errors.New("cannot read sealed watch recovery values")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 || watchValuesDigest(raw) != p.ValuesSHA256 {
		return errors.New("sealed watch recovery values differ from the approved plan")
	}
	validated, err := readWatchValuesMode(cfg.ValuesFiles, true)
	if err != nil || !bytes.Equal(raw, validated) {
		return errors.New("sealed watch recovery values are not canonical approved public inputs")
	}
	return nil
}
