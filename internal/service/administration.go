package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// Administration supplies the runtime adapters owned by the server entrypoint.
// Every adapter receives validated values after the admin and audit gates. No
// adapter accepts command arguments, filesystem paths or a Kubernetes context.
type Administration struct {
	TokenPolicy       model.UplinkTokenPolicy
	DatabaseBackup    func(context.Context, io.Writer, int64) error
	Version           string
	EnsureUpstream    func(context.Context, api.UpstreamAddRequest) (api.UpstreamIdentity, error)
	ImportUpstreamKey func(context.Context, api.UpstreamKeyRequest) (api.UpstreamIdentity, error)
	CheckUpstream     func(context.Context, model.Upstream) (string, error)
	VerifyRepository  func(context.Context, string) (any, error)
}

type adminCatalog interface {
	ListUpstreams(context.Context) ([]model.Upstream, error)
	RegisterUpstream(context.Context, string, model.UpstreamSpec) (model.Upstream, error)
	RemoveUpstream(context.Context, string, string) ([]string, error)
	RegisterRepository(context.Context, string, model.RepositorySpec) (model.Repository, error)
	RepositoryByName(context.Context, string) (model.Repository, error)
}

func (s *API) administrationTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (value any, resultErr error) {
	if !actor.Admin || actor.Scoped() || strings.TrimSpace(actor.Identity) == "" {
		return nil, refuse(ErrForbidden, "administration requires an unscoped admin identity")
	}
	// These reads also append audit entries: fail closed on an unavailable gate.
	if err := s.requireMutation(ctx); err != nil {
		return nil, err
	}
	if s.auditor == nil {
		return nil, refuse(ErrUnavailable, "administration audit")
	}
	ctx = model.WithAuthMethod(ctx, actor.AuthMethod)
	resourceType, resourceID := administrationAuditTarget(name, raw)
	if _, err := s.auditor.AppendAuditAction(ctx, model.AuditActionSpec{Actor: actor.Identity, Action: name + ".intent", ResourceType: resourceType, ResourceID: resourceID, Details: "{}"}); err != nil {
		return nil, err
	}
	defer func() {
		outcome := ".succeeded"
		if resultErr != nil {
			outcome = ".failed"
		}
		// Retain actor/auth metadata and bound outcome persistence after cancellation.
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, err := s.auditor.AppendAuditAction(auditCtx, model.AuditActionSpec{Actor: actor.Identity, Action: name + outcome, ResourceType: resourceType, ResourceID: resourceID, Details: "{}"})
		resultErr = errors.Join(resultErr, err)
		if resultErr != nil {
			if stream, ok := value.(io.Closer); ok {
				_ = stream.Close()
			}
			value = nil
		}
	}()
	if api.ValidateAdminObject(raw) != nil {
		return nil, refuse(ErrInvalidInput, "administration requires a bounded JSON object")
	}
	return s.performAdministration(ctx, actor, name, raw)
}

func (s *API) performAdministration(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "uplink_add":
		return s.adminUplinkAdd(ctx, actor, raw)
	case "uplink_scope":
		return s.adminUplinkScope(ctx, actor, raw)
	case "admin_database_backup":
		return s.stageAdminBackup(ctx, raw)
	case "admin_public_identity":
		var args struct {
			Name string `json:"name"`
		}
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		body, err := readAdminPublicIdentity(ctx, args.Name)
		if err != nil {
			return nil, err
		}
		return map[string]string{"name": args.Name, "content": string(body)}, nil
	case "admin_readiness", "admin_version", "admin_access_list":
		var args struct{}
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		switch name {
		case "admin_readiness":
			return model.Readiness(s.Ready(ctx)), nil
		case "admin_version":
			return map[string]any{"version": s.administration.Version, "schema": store.LatestMigrationVersion()}, nil
		default:
			return s.adminAccessList(ctx)
		}
	case "admin_idle":
		var args struct {
			WaitFor string `json:"wait_for"`
		}
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		if args.WaitFor == "" {
			args.WaitFor = "all"
		}
		if args.WaitFor != "all" && args.WaitFor != "critical" {
			return nil, refuse(ErrInvalidInput, "wait_for must be all or critical")
		}
		reader, ok := s.runs.(interface {
			IdleQueueStatus(context.Context) (model.IdleQueueStatus, error)
		})
		if !ok {
			return nil, refuse(ErrUnavailable, "idle observation")
		}
		state, err := reader.IdleQueueStatus(ctx)
		if err != nil {
			return nil, err
		}
		busy := state.Queued+state.Running > 0
		if args.WaitFor == "critical" {
			busy = state.Critical() > 0
		}
		return map[string]any{"busy": busy, "wait_for": args.WaitFor, "queue": state}, nil
	case "admin_audit_record":
		var args struct {
			Action  string          `json:"action"`
			Details json.RawMessage `json:"details"`
		}
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		if args.Action != "backup.completed" && args.Action != "restore.completed" && args.Action != "upgrade.completed" {
			return nil, refuse(ErrInvalidInput, "unknown lifecycle action")
		}
		if len(args.Details) == 0 {
			args.Details = json.RawMessage("{}")
		}
		if !bytes.HasPrefix(bytes.TrimSpace(args.Details), []byte("{")) {
			return nil, refuse(ErrInvalidInput, "details must be an object")
		}
		return s.auditor.AppendAuditAction(ctx, model.AuditActionSpec{Actor: actor.Identity, Action: args.Action, ResourceType: "system", ResourceID: "oberth", Details: string(args.Details)})
	case "upstream_provide_key":
		var args api.UpstreamKeyRequest
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		if err := validateAdminUpstreamName(args.Name); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		if len(args.PrivateKey) == 0 || len(args.PrivateKey) > 64<<10 {
			return nil, refuse(ErrInvalidInput, "private_key must contain at most 64 KiB of key material")
		}
		if _, err := ssh.ParsePrivateKey([]byte(args.PrivateKey)); err != nil {
			return nil, refuse(ErrInvalidInput, "private_key must be an unencrypted SSH private key")
		}
		if s.administration.ImportUpstreamKey == nil {
			return nil, refuse(ErrUnavailable, "upstream identity writer")
		}
		return s.administration.ImportUpstreamKey(ctx, args)
	case "repo_verify":
		var args struct {
			Repo string `json:"repo"`
		}
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		if err := validateAdminRepo(args.Repo); err != nil {
			return nil, err
		}
		if s.administration.VerifyRepository == nil {
			return nil, refuse(ErrUnavailable, "repository verification")
		}
		repository, err := s.runs.RepositoryByName(ctx, args.Repo)
		if err != nil {
			return nil, err
		}
		qualified, err := s.repositoryIdentity(ctx, repository.ID)
		if err != nil {
			return nil, err
		}
		return s.administration.VerifyRepository(ctx, qualified)
	}
	return s.performCatalogAdministration(ctx, actor, name, raw)
}

func (s *API) performCatalogAdministration(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	catalog, ok := s.runs.(adminCatalog)
	if !ok {
		return nil, refuse(ErrUnavailable, "administration catalog")
	}
	switch name {
	case "upstream_list":
		var args struct {
			Check bool `json:"check"`
		}
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		values, err := catalog.ListUpstreams(ctx)
		if err != nil {
			return nil, err
		}
		rows := make([]map[string]any, 0, len(values))
		for _, upstream := range values {
			row := map[string]any{"upstream": upstream}
			if args.Check {
				if s.administration.CheckUpstream == nil {
					return nil, refuse(ErrUnavailable, "upstream verification")
				}
				status, err := s.administration.CheckUpstream(ctx, upstream)
				if err != nil {
					return nil, err
				}
				row["auth"] = status
			}
			rows = append(rows, row)
		}
		return map[string]any{"upstreams": rows}, nil
	case "upstream_add":
		var args api.UpstreamAddRequest
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		if err := validateAdminUpstream(args); err != nil {
			return nil, err
		}
		parsed, _ := url.Parse(args.BaseURL)
		keyName := ""
		if args.DedicatedKey {
			keyName = "id_ed25519-" + args.Name
		}
		spec := model.UpstreamSpec{Name: args.Name, Kind: parsed.Scheme, BaseURL: args.BaseURL, KeyName: keyName}
		values, err := catalog.ListUpstreams(ctx)
		if err != nil {
			return nil, err
		}
		var existing *model.Upstream
		for i := range values {
			if values[i].Name != args.Name {
				continue
			}
			if values[i].Kind != spec.Kind || values[i].BaseURL != spec.BaseURL || values[i].KeyName != spec.KeyName {
				return nil, refuse(ErrInvalidInput, "upstream already has a different registration")
			}
			existing = &values[i]
		}
		var identity any
		if parsed.Scheme == "ssh" {
			if s.administration.EnsureUpstream == nil {
				return nil, refuse(ErrUnavailable, "upstream identity bootstrap")
			}
			identity, err = s.administration.EnsureUpstream(ctx, args)
			if err != nil {
				return nil, err
			}
		}
		if existing != nil {
			return map[string]any{"upstream": *existing, "identity": identity}, nil
		}
		upstream, err := catalog.RegisterUpstream(ctx, actor.Identity, spec)
		if err != nil {
			return nil, err
		}
		return map[string]any{"upstream": upstream, "identity": identity}, nil
	case "upstream_remove":
		var args struct {
			Name string `json:"name"`
		}
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		if err := validateAdminUpstreamName(args.Name); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		removed, err := catalog.RemoveUpstream(ctx, actor.Identity, args.Name)
		return map[string]any{"removed_repositories": removed}, err
	case "repo_add":
		var args api.RepoAddRequest
		if err := decodeAdministration(raw, &args); err != nil {
			return nil, err
		}
		if err := validateAdminRepo(args.Repo); err != nil {
			return nil, err
		}
		if err := validateAdminUpstreamName(args.Upstream); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		if args.DefaultBranch == "" {
			args.DefaultBranch = "main"
		}
		if strings.Contains(args.DefaultBranch, "%") {
			return nil, refuse(ErrInvalidInput, "default_branch must not use URL escapes")
		}
		if err := gitcache.ValidateBranch(args.DefaultBranch); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
		pathUpstream, org, repo, _ := gitcache.ParseRepoPath(args.Repo)
		values, err := catalog.ListUpstreams(ctx)
		if err != nil {
			return nil, err
		}
		for _, upstream := range values {
			if upstream.Name != args.Upstream {
				continue
			}
			if pathUpstream != "" && pathUpstream != upstream.Name || org != "" && org != upstream.Org() {
				return nil, refuse(ErrInvalidInput, "repository path does not belong to upstream")
			}
			existing, err := catalog.RepositoryByName(ctx, upstream.QualifiedRepo(repo))
			if err == nil {
				return existing, nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			return catalog.RegisterRepository(ctx, actor.Identity, model.RepositorySpec{Name: repo, UpstreamID: upstream.ID, DefaultBranch: args.DefaultBranch})
		}
		return nil, refuse(ErrInvalidInput, "upstream is not registered")
	}
	return nil, refuse(ErrInvalidInput, "unknown administration action")
}

func validateAdminRepo(value string) error {
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "%") {
		return refuse(ErrInvalidInput, "repository must be a relative repository identity")
	}
	if _, _, _, err := gitcache.ParseRepoPath(value); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	return nil
}

func validateAdminUpstream(args api.UpstreamAddRequest) error {
	if err := validateAdminUpstreamName(args.Name); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	u, err := url.Parse(args.BaseURL)
	if err != nil || u == nil || u.Scheme != "ssh" || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(args.BaseURL, "/") || strings.HasSuffix(strings.ToLower(u.Path), ".git") {
		return refuse(ErrInvalidInput, "base_url must be an SSH repository namespace URL")
	}
	if err := gitcache.ValidateUpstream(args.BaseURL + "/oberth-probe"); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	for index, segment := range strings.Split(strings.TrimPrefix(u.Path, "/"), "/") {
		if index == 0 && segment == "~" {
			continue
		}
		if err := gitcache.ValidateSegment("upstream namespace", segment); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
	}
	if args.CheckRepository != "" {
		if err := gitcache.ValidateSegment("repository", args.CheckRepository); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
	}
	if args.ExpectedHostFingerprint != "" {
		fingerprint, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(args.ExpectedHostFingerprint, "SHA256:"))
		if !strings.HasPrefix(args.ExpectedHostFingerprint, "SHA256:") || err != nil || len(fingerprint) != 32 {
			return refuse(ErrInvalidInput, "invalid expected host fingerprint")
		}
	}
	if u.Scheme != "ssh" && (args.DedicatedKey || args.Repin || args.ExpectedHostFingerprint != "" || args.CheckRepository != "" || args.NoWait) {
		return refuse(ErrInvalidInput, "SSH bootstrap options require an SSH upstream")
	}
	return nil
}

func (s *API) adminAccessList(ctx context.Context) (any, error) {
	if s.secretAccess == nil {
		return nil, refuse(ErrUnavailable, "secret access")
	}
	grants, err := s.secretAccess.SecretAccessList(ctx, "", false)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]string, 0, len(grants))
	for _, grant := range grants {
		repo := grant.Repo
		rows = append(rows, map[string]string{"repo": repo, "step": grant.Step, "secret": grant.Secret})
	}
	return map[string]any{"grants": rows}, nil
}

// Administrative JSON uses the exact schema spellings; the general decoder's
// case-insensitive field matching must not create alternate argument names.
func decodeAdministration(raw json.RawMessage, value any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return refuse(ErrInvalidInput, "invalid administration object")
	}
	for key := range fields {
		accepted := false
		for _, name := range toolArgumentNames(value) {
			if key == name {
				accepted = true
				break
			}
		}
		if !accepted {
			return refuse(ErrInvalidInput, "unknown administration argument")
		}
	}
	return decodeTool(raw, value)
}

func validateAdminUpstreamName(value string) error {
	if len(value) > 53 || len(validation.IsDNS1123Label(value)) != 0 {
		return errors.New("upstream name must be at most 53 lowercase letters, digits or hyphens, with alphanumeric ends")
	}
	return gitcache.ValidateUpstreamName(value)
}

// Only validated public identifiers enter the envelope; key bytes and arbitrary
// request contents are excluded even for malformed or rejected operations.
func administrationAuditTarget(name string, raw json.RawMessage) (string, string) {
	if api.ValidateAdminObject(raw) != nil {
		return "system", "oberth"
	}
	var target struct {
		Name string `json:"name"`
		Repo string `json:"repo"`
	}
	if json.Unmarshal(raw, &target) != nil {
		return "system", "oberth"
	}
	if strings.HasPrefix(name, "upstream_") && validateAdminUpstreamName(target.Name) == nil {
		return "upstream", target.Name
	}
	if (name == "repo_add" || name == "repo_verify") && validateAdminRepo(target.Repo) == nil {
		return "repository", target.Repo
	}
	if name == "admin_public_identity" {
		switch target.Name {
		case "server-ca", "server-certificate", "upstream-public-key":
			return "public_identity", target.Name
		}
	}
	return "system", "oberth"
}

func (s *API) adminUplinkScope(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	var args struct {
		Identity    string    `json:"identity"`
		Fingerprint string    `json:"fingerprint"`
		Scope       *[]string `json:"scope"`
	}
	if err := decodeAdministration(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Identity) > 255 || !strings.Contains(args.Identity, "@") || strings.IndexFunc(args.Identity, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return nil, refuse(ErrInvalidInput, "identity must have the form identity@host")
	}
	if args.Fingerprint != "" {
		fingerprint, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(args.Fingerprint, "SHA256:"))
		if !strings.HasPrefix(args.Fingerprint, "SHA256:") || err != nil || len(fingerprint) != 32 {
			return nil, refuse(ErrInvalidInput, "invalid SSH public-key fingerprint")
		}
	}
	if args.Scope == nil || len(*args.Scope) > 128 {
		return nil, refuse(ErrInvalidInput, "scope must be an array of at most 128 upstream names")
	}
	for _, name := range *args.Scope {
		if name == model.MetricsScope {
			continue
		}
		if err := validateAdminUpstreamName(name); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
		}
	}
	store, ok := s.runs.(interface {
		ResolveUplinkScopes(context.Context, []string) ([]string, error)
		SetUplinkScope(context.Context, string, string, string, []string) (model.Uplink, error)
	})
	if !ok {
		return nil, refuse(ErrUnavailable, "uplink scope")
	}
	scopes, err := store.ResolveUplinkScopes(ctx, *args.Scope)
	if err != nil {
		return nil, err
	}
	value, err := store.SetUplinkScope(ctx, actor.Identity, args.Identity, args.Fingerprint, scopes)
	if err != nil {
		return nil, err
	}
	return map[string]any{"identity": value.Identity, "fingerprint": value.Fingerprint, "scope": append([]string{}, value.Scope...)}, nil
}
