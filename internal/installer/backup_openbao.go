package installer

// OpenBao half of `oberth backup` / `oberth restore`. Every call goes through
// openBaoExec.authenticated: the root token travels as the first stdin line
// and every secret body as the rest of stdin — never argv, which the API
// server records verbatim in its exec audit log (issue 038).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Bounds on what one backup walks; an OpenBao holding more than this under
// the oberth mount is not an Oberth store this tool understands.
const (
	maxBackupKVSecrets = 20000
	maxBackupKVDepth   = 32
)

type kvSecretExport struct {
	Path string         `json:"path"`
	Data map[string]any `json:"data"`
}

// installIdentityPrefix holds the runtime identities every install makes for
// itself (forge key, server TLS and SSH host keys, forge host keys). They are
// never backed up or restored: a restored install keeps its own.
const installIdentityPrefix = "identities/"

// operatorSecrets drops the install's own identities from a KV export.
func operatorSecrets(secrets []kvSecretExport) []kvSecretExport {
	kept := make([]kvSecretExport, 0, len(secrets))
	for _, secret := range secrets {
		if !strings.HasPrefix(secret.Path, installIdentityPrefix) {
			kept = append(kept, secret)
		}
	}
	return kept
}

type kvExport struct {
	Mount   string           `json:"mount"`
	Secrets []kvSecretExport `json:"secrets"`
	// SkippedDeleted lists paths whose current version is deleted or
	// destroyed: there is no value to carry over.
	SkippedDeleted []string `json:"skipped_deleted"`
}

type policyExport struct {
	Name  string `json:"name"`
	Rules string `json:"rules"`
}

type roleExport struct {
	Name   string         `json:"name"`
	Fields map[string]any `json:"fields"`
}

type rolesExport struct {
	Mount string       `json:"mount"`
	Roles []roleExport `json:"roles"`
}

// openBaoReference is recorded for the operator only and never restored.
type openBaoReference struct {
	AuthConfig   map[string]any            `json:"auth_config"`
	AuthMounts   map[string]baoMount       `json:"auth_mounts"`
	SecretMounts map[string]baoMount       `json:"secret_mounts"`
	TransitKeys  map[string]map[string]any `json:"transit_keys"`
	PKICACerts   map[string]string         `json:"pki_ca_certificates"`
	Warnings     []string                  `json:"warnings"`
}

// builtinPolicies are owned by OpenBao itself and never exported.
var builtinPolicies = map[string]bool{"root": true, "default": true}

// list returns the keys at path, or nil when the path holds nothing. Folder
// keys keep their trailing slash.
func (b openBaoExec) list(ctx context.Context, token, path string) ([]string, error) {
	out, err := b.authenticated(ctx, token, nil, "list", "-format=json", path)
	if err != nil {
		if bytes.Contains(out, []byte("No value found")) {
			return nil, nil
		}
		return nil, fmt.Errorf("bao list %s: %w%s", path, err, commandOutputSuffix(out))
	}
	start := bytes.IndexByte(out, '[')
	end := bytes.LastIndexByte(out, ']')
	if start < 0 || end < start {
		return nil, fmt.Errorf("bao list %s: no JSON array in output", path)
	}
	var keys []string
	if err := json.Unmarshal(out[start:end+1], &keys); err != nil {
		return nil, fmt.Errorf("bao list %s: parse output: %w", path, err)
	}
	return keys, nil
}

// exportKV walks the KV v2 mount recursively and reads the current version of
// every secret. Errors name paths, never values.
func exportKV(ctx context.Context, store openBaoExec, token, mount string) (kvExport, error) {
	export := kvExport{Mount: mount, Secrets: []kvSecretExport{}, SkippedDeleted: []string{}}
	var walk func(prefix string, depth int) error
	walk = func(prefix string, depth int) error {
		if depth > maxBackupKVDepth {
			return fmt.Errorf("KV path %s/%s nests deeper than %d levels", mount, prefix, maxBackupKVDepth)
		}
		keys, err := store.list(ctx, token, mount+"/metadata/"+prefix)
		if err != nil {
			return err
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
				return fmt.Errorf("KV mount %s lists an unsafe key under %q", mount, prefix)
			}
			if strings.HasSuffix(key, "/") {
				if err := walk(prefix+key, depth+1); err != nil {
					return err
				}
				continue
			}
			path := prefix + key
			envelope, err := store.readData(ctx, token, mount+"/data/"+path)
			if err != nil {
				return err
			}
			data, _ := envelope["data"].(map[string]any)
			if data == nil {
				export.SkippedDeleted = append(export.SkippedDeleted, path)
				continue
			}
			export.Secrets = append(export.Secrets, kvSecretExport{Path: path, Data: data})
			if len(export.Secrets) > maxBackupKVSecrets {
				return fmt.Errorf("KV mount %s holds more than %d secrets", mount, maxBackupKVSecrets)
			}
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return kvExport{}, err
	}
	return export, nil
}

// exportPolicies reads every ACL policy except OpenBao's built-ins.
func exportPolicies(ctx context.Context, store openBaoExec, token string) ([]policyExport, error) {
	names, err := store.policyList(ctx, token)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	policies := []policyExport{}
	for _, name := range names {
		if builtinPolicies[name] {
			continue
		}
		rules, exists, err := store.policyRead(ctx, token, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue // deleted between list and read
		}
		policies = append(policies, policyExport{Name: name, Rules: rules})
	}
	return policies, nil
}

// exportRoles reads every Kubernetes auth role with every field the server
// returns. A hand-picked field subset once dropped token_no_default_policy
// from 42 restored roles (issue 038); the export never selects fields.
func exportRoles(ctx context.Context, store openBaoExec, token, mount string) (rolesExport, error) {
	export := rolesExport{Mount: mount, Roles: []roleExport{}}
	names, err := store.list(ctx, token, "auth/"+mount+"/role")
	if err != nil {
		return rolesExport{}, err
	}
	sort.Strings(names)
	for _, name := range names {
		fields, err := store.readData(ctx, token, "auth/"+mount+"/role/"+name)
		if err != nil {
			return rolesExport{}, err
		}
		if fields == nil {
			continue
		}
		export.Roles = append(export.Roles, roleExport{Name: name, Fields: fields})
	}
	return export, nil
}

// exportOpenBaoReference records cluster-specific and non-exportable state.
// Failures here are warnings: the reference is informative, not restorable.
func exportOpenBaoReference(ctx context.Context, store openBaoExec, token string) openBaoReference {
	ref := openBaoReference{TransitKeys: map[string]map[string]any{}, PKICACerts: map[string]string{}, Warnings: []string{}}
	warn := func(err error) { ref.Warnings = append(ref.Warnings, err.Error()) }
	var err error
	if ref.AuthConfig, err = store.readData(ctx, token, "auth/"+defaultAuthMount+"/config"); err != nil {
		warn(err)
	}
	if ref.AuthMounts, err = store.listMounts(ctx, token, "auth"); err != nil {
		warn(err)
	}
	if ref.SecretMounts, err = store.listMounts(ctx, token, "secrets"); err != nil {
		warn(err)
	}
	mounts := make([]string, 0, len(ref.SecretMounts))
	for path := range ref.SecretMounts {
		mounts = append(mounts, path)
	}
	sort.Strings(mounts)
	for _, path := range mounts {
		mount := strings.TrimSuffix(path, "/")
		switch ref.SecretMounts[path].Type {
		case "transit":
			keys, err := store.list(ctx, token, mount+"/keys")
			if err != nil {
				warn(err)
				continue
			}
			for _, key := range keys {
				// Key metadata (type, versions, exportable=false) only: the
				// key material itself cannot leave OpenBao.
				meta, err := store.readData(ctx, token, mount+"/keys/"+key)
				if err != nil {
					warn(err)
					continue
				}
				ref.TransitKeys[mount+"/"+key] = meta
			}
		case "pki":
			ca, err := store.readData(ctx, token, mount+"/cert/ca")
			if err != nil {
				warn(err)
				continue
			}
			if certificate, ok := ca["certificate"].(string); ok {
				ref.PKICACerts[mount] = certificate
			}
		}
	}
	return ref
}

// deprecatedRoleAliases maps the pre-tokenutil role parameters OpenBao still
// echoes on read to their token_* successors. Writing both spellings is
// rejected ("cannot specify both"), so an alias that merely mirrors its
// successor is dropped from the write and from the read-back comparison;
// an alias with a distinct value is written as-is.
var deprecatedRoleAliases = map[string]string{
	"policies":    "token_policies",
	"ttl":         "token_ttl",
	"max_ttl":     "token_max_ttl",
	"period":      "token_period",
	"num_uses":    "token_num_uses",
	"bound_cidrs": "token_bound_cidrs",
}

// roleWritePayload returns every field of an exported role except
// deprecated aliases that mirror their successor.
func roleWritePayload(fields map[string]any) map[string]any {
	payload := make(map[string]any, len(fields))
	for key, value := range fields {
		if successor, alias := deprecatedRoleAliases[key]; alias {
			if current, ok := fields[successor]; ok && canonicalJSON(current) == canonicalJSON(value) {
				continue
			}
		}
		payload[key] = value
	}
	return payload
}

// diffRole lists the archived fields whose read-back value differs.
func diffRole(want, have map[string]any) []string {
	// Only archived fields are compared: a newer target that echoes an extra
	// field at its default value is not a difference the restore introduced.
	wantPayload, havePayload := roleWritePayload(want), roleWritePayload(have)
	var differing []string
	for key := range wantPayload {
		if canonicalJSON(wantPayload[key]) != canonicalJSON(havePayload[key]) {
			differing = append(differing, key)
		}
	}
	sort.Strings(differing)
	return differing
}

// canonicalJSON renders a decoded JSON value in one normalized spelling, so
// json.Number from the archive and float64 from a bao read compare equal.
func canonicalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("unencodable:%T", value)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return string(encoded)
	}
	out, err := json.Marshal(normalized)
	if err != nil {
		return string(encoded)
	}
	return string(out)
}

// openBaoRestorePlan is what a restore will change in OpenBao.
type openBaoRestorePlan struct {
	SecretsNew, SecretsReplaced int
	// NewPaths and ReplacedPaths name the secrets (never their values).
	NewPaths, ReplacedPaths []string
	// SecretsUnchanged already hold the archived value; restore skips them,
	// so it adds no KV version.
	SecretsUnchanged  int
	PoliciesNew       []string
	PoliciesReplaced  []string
	PoliciesUnchanged int
	RolesNew          []string
	RolesReplaced     []string
	RolesUnchanged    int
	// Known is false when no root token was available to read the target
	// (dry run only); the counts are then the archive's own.
	Known bool
}

// planOpenBaoRestore compares the archive with the target store. Secret
// values are read only to compare them in memory; the plan carries counts.
func planOpenBaoRestore(ctx context.Context, store openBaoExec, token string, kv kvExport, policies []policyExport, roles rolesExport) (openBaoRestorePlan, error) {
	plan := openBaoRestorePlan{Known: true}
	existing := map[string]bool{}
	var walk func(prefix string, depth int) error
	walk = func(prefix string, depth int) error {
		if depth > maxBackupKVDepth {
			return nil
		}
		keys, err := store.list(ctx, token, kv.Mount+"/metadata/"+prefix)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if strings.HasSuffix(key, "/") {
				if err := walk(prefix+key, depth+1); err != nil {
					return err
				}
				continue
			}
			existing[prefix+key] = true
		}
		return nil
	}
	if err := walk("", 0); err != nil {
		return plan, err
	}
	for _, secret := range kv.Secrets {
		if !existing[secret.Path] {
			plan.SecretsNew++
			plan.NewPaths = append(plan.NewPaths, secret.Path)
			continue
		}
		same, err := secretUnchanged(ctx, store, token, kv.Mount, secret)
		if err != nil {
			return plan, err
		}
		if same {
			plan.SecretsUnchanged++
		} else {
			plan.SecretsReplaced++
			plan.ReplacedPaths = append(plan.ReplacedPaths, secret.Path)
		}
	}
	for _, policy := range policies {
		have, exists, err := store.policyRead(ctx, token, policy.Name)
		if err != nil {
			return plan, err
		}
		switch {
		case !exists:
			plan.PoliciesNew = append(plan.PoliciesNew, policy.Name)
		case strings.TrimSpace(have) != strings.TrimSpace(policy.Rules):
			plan.PoliciesReplaced = append(plan.PoliciesReplaced, policy.Name)
		default:
			plan.PoliciesUnchanged++
		}
	}
	for _, role := range roles.Roles {
		have, err := store.readData(ctx, token, "auth/"+roles.Mount+"/role/"+role.Name)
		if err != nil {
			return plan, err
		}
		switch {
		case have == nil:
			plan.RolesNew = append(plan.RolesNew, role.Name)
		case len(diffRole(role.Fields, have)) > 0:
			plan.RolesReplaced = append(plan.RolesReplaced, role.Name)
		default:
			plan.RolesUnchanged++
		}
	}
	return plan, nil
}

// secretUnchanged reports whether the current version of secret already holds
// the archived data. A deleted or destroyed latest version counts as changed.
func secretUnchanged(ctx context.Context, store openBaoExec, token, mount string, secret kvSecretExport) (bool, error) {
	envelope, err := store.readData(ctx, token, mount+"/data/"+secret.Path)
	if err != nil || envelope == nil {
		return false, err
	}
	have, _ := envelope["data"].(map[string]any)
	return have != nil && canonicalJSON(have) == canonicalJSON(secret.Data), nil
}

// restoreOpenBao writes every archived secret, policy and role, then reads
// each one back and compares it with the archive. A secret that already holds
// the archived value is not written again. Secrets are compared in full; a
// mismatch is reported by path only. It returns how many secrets it wrote.
func restoreOpenBao(ctx context.Context, store openBaoExec, token string, kv kvExport, policies []policyExport, roles rolesExport) (int, error) {
	written := 0
	for _, secret := range kv.Secrets {
		same, err := secretUnchanged(ctx, store, token, kv.Mount, secret)
		if err != nil {
			return written, err
		}
		if same {
			continue
		}
		if err := store.writeJSON(ctx, token, kv.Mount+"/data/"+secret.Path, map[string]any{"data": secret.Data}); err != nil {
			return written, err
		}
		written++
	}
	for _, policy := range policies {
		if err := store.policyWrite(ctx, token, policy.Name, policy.Rules); err != nil {
			return written, err
		}
	}
	for _, role := range roles.Roles {
		if err := store.writeJSON(ctx, token, "auth/"+roles.Mount+"/role/"+role.Name, roleWritePayload(role.Fields)); err != nil {
			return written, err
		}
	}

	var mismatches []string
	for _, secret := range kv.Secrets {
		envelope, err := store.readData(ctx, token, kv.Mount+"/data/"+secret.Path)
		if err != nil {
			return written, err
		}
		have, _ := envelope["data"].(map[string]any)
		if canonicalJSON(have) != canonicalJSON(secret.Data) {
			mismatches = append(mismatches, "secret "+kv.Mount+"/"+secret.Path)
		}
	}
	for _, policy := range policies {
		have, exists, err := store.policyRead(ctx, token, policy.Name)
		if err != nil {
			return written, err
		}
		if !exists || strings.TrimSpace(have) != strings.TrimSpace(policy.Rules) {
			mismatches = append(mismatches, "policy "+policy.Name)
		}
	}
	for _, role := range roles.Roles {
		have, err := store.readData(ctx, token, "auth/"+roles.Mount+"/role/"+role.Name)
		if err != nil {
			return written, err
		}
		if have == nil {
			mismatches = append(mismatches, "role "+role.Name+" (absent)")
			continue
		}
		if fields := diffRole(role.Fields, have); len(fields) > 0 {
			mismatches = append(mismatches, "role "+role.Name+" ("+strings.Join(fields, ", ")+")")
		}
	}
	if len(mismatches) > 0 {
		return written, errors.New("OpenBao read-back differs from the archive for: " + strings.Join(mismatches, "; "))
	}
	return written, nil
}
