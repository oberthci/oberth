package argojob

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync/atomic"

	"github.com/oberthci/oberth/internal/model"
)

// ReleaseWIFRuntime layers administrator runtime overrides (#1497) on the boot
// capability map. The boot map stays the default; the server-owned store
// holds every override, so a Helm upgrade and a runtime change never edit the
// same object. Roles, the namespace and dedicated step identities remain boot
// configuration: a runtime grant can only bind an existing role to a
// repository's own release ServiceAccount. Published snapshots are immutable.
type ReleaseWIFRuntime struct {
	base       Config
	current    atomic.Pointer[releaseWIFRuntimeState]
	identities atomic.Pointer[IdentityStore]
}

type releaseWIFRuntimeState struct {
	config   *ReleaseWIFConfig
	inactive []model.ReleaseWIFOverrideStatus
}

// NewReleaseWIFRuntime freezes a copy of the boot configuration.
func NewReleaseWIFRuntime(config Config) *ReleaseWIFRuntime {
	config.ReleaseWIF = config.ReleaseWIF.Clone()
	config.ReleaseTagRoutes = config.ReleaseTagRoutes.Clone()
	runtime := &ReleaseWIFRuntime{base: config}
	runtime.current.Store(&releaseWIFRuntimeState{config: config.ReleaseWIF.Clone()})
	return runtime
}

// SetIdentityStore makes grant validation read the live per-repository
// identities, the same maps admission authorizes against.
func (runtime *ReleaseWIFRuntime) SetIdentityStore(store *IdentityStore) {
	runtime.identities.Store(store)
}

// Base returns a copy of the boot capability map.
func (runtime *ReleaseWIFRuntime) Base() *ReleaseWIFConfig {
	return runtime.base.ReleaseWIF.Clone()
}

// Snapshot returns the effective capability map for one admission. Callers
// pin it in Request.ReleaseWIF so the audit and submission Builds agree.
func (runtime *ReleaseWIFRuntime) Snapshot() *ReleaseWIFConfig {
	return runtime.current.Load().config.Clone()
}

// State describes the boot and effective maps. Revocations always apply
// because removing an entry only narrows authority; invalid grants are listed.
func (runtime *ReleaseWIFRuntime) State() model.ReleaseWIFState {
	state := runtime.current.Load()
	view := model.ReleaseWIFState{Enabled: runtime.base.ReleaseWIF != nil, Boot: map[string]json.RawMessage{}, Effective: map[string]json.RawMessage{}, Roles: []string{}}
	view.Inactive = append(view.Inactive, state.inactive...)
	encodeReleaseWIFRepositories(view.Boot, runtime.base.ReleaseWIF)
	encodeReleaseWIFRepositories(view.Effective, state.config)
	if runtime.base.ReleaseWIF != nil {
		for role := range runtime.base.ReleaseWIF.Roles {
			view.Roles = append(view.Roles, role)
		}
		slices.Sort(view.Roles)
	}
	return view
}

// Check refuses a candidate override set unless every grant applies cleanly.
func (runtime *ReleaseWIFRuntime) Check(overrides []model.ReleaseWIFOverride) error {
	_, inactive := runtime.Preview(overrides)
	if len(inactive) > 0 {
		return fmt.Errorf("release WIF override for %s: %s", inactive[0].Repo, inactive[0].Error)
	}
	return nil
}

// Preview returns the effective map a candidate override set would publish,
// encoded like State, and the grants it would leave inactive. It changes
// nothing.
func (runtime *ReleaseWIFRuntime) Preview(overrides []model.ReleaseWIFOverride) (map[string]json.RawMessage, []model.ReleaseWIFOverrideStatus) {
	config, inactive := runtime.merge(overrides)
	effective := map[string]json.RawMessage{}
	encodeReleaseWIFRepositories(effective, config)
	return effective, inactive
}

func encodeReleaseWIFRepositories(target map[string]json.RawMessage, config *ReleaseWIFConfig) {
	if config == nil {
		return
	}
	for repo, grant := range config.Repositories {
		if body, err := json.Marshal(grant); err == nil {
			target[repo] = body
		}
	}
}

// Apply publishes the effective map. Invalid stored grants are skipped and
// reported; they never fail startup or widen authority.
func (runtime *ReleaseWIFRuntime) Apply(overrides []model.ReleaseWIFOverride) []model.ReleaseWIFOverrideStatus {
	config, inactive := runtime.merge(overrides)
	runtime.current.Store(&releaseWIFRuntimeState{config: config, inactive: inactive})
	return append([]model.ReleaseWIFOverrideStatus(nil), inactive...)
}

func (runtime *ReleaseWIFRuntime) merge(overrides []model.ReleaseWIFOverride) (*ReleaseWIFConfig, []model.ReleaseWIFOverrideStatus) {
	merged := runtime.base.ReleaseWIF.Clone()
	var inactive []model.ReleaseWIFOverrideStatus
	if merged == nil {
		for _, override := range overrides {
			if !override.Revoked {
				inactive = append(inactive, model.ReleaseWIFOverrideStatus{Repo: override.Repo, Error: "release WIF is not configured on this server"})
			}
		}
		return nil, inactive
	}
	if merged.Repositories == nil {
		merged.Repositories = map[string]ReleaseWIFRepository{}
	}
	base := runtime.base
	if store := runtime.identities.Load(); store != nil {
		if snapshot := store.Snapshot(); snapshot != nil {
			base.PerRepoIdentities, base.PerRepoCIIdentities = snapshot.Release, snapshot.CI
		}
	}
	for _, override := range overrides {
		if override.Revoked {
			delete(merged.Repositories, override.Repo)
		}
	}
	for _, override := range overrides {
		if override.Revoked {
			continue
		}
		grant, err := decodeRuntimeReleaseWIFGrant(override.Grant)
		if err == nil {
			candidate := merged.Clone()
			candidate.Repositories[override.Repo] = grant
			config := base
			config.ReleaseWIF = candidate
			if err = config.validateRuntimeReleaseWIFGrant(override.Repo, grant); err == nil {
				merged = candidate
			}
		}
		if err != nil {
			inactive = append(inactive, model.ReleaseWIFOverrideStatus{Repo: override.Repo, Error: err.Error()})
		}
	}
	return merged, inactive
}

// decodeRuntimeReleaseWIFGrant accepts only the repository ServiceAccount and
// template roles. Dedicated step accounts need Kubernetes objects and exact
// route pins that only the deployment configuration can create.
func decodeRuntimeReleaseWIFGrant(raw json.RawMessage) (ReleaseWIFRepository, error) {
	var grant struct {
		ServiceAccountName string            `json:"service_account_name"`
		Templates          map[string]string `json:"templates"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grant); err != nil {
		return ReleaseWIFRepository{}, errors.New("runtime grant accepts only service_account_name and templates")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ReleaseWIFRepository{}, errors.New("runtime grant must be one JSON object")
	}
	if len(grant.Templates) == 0 {
		return ReleaseWIFRepository{}, errors.New("runtime grant requires at least one template role")
	}
	return ReleaseWIFRepository{ServiceAccountName: grant.ServiceAccountName, Templates: grant.Templates}, nil
}

// The merged map must pass the same validation as a boot map. A runtime grant
// also needs a repository ServiceAccount (it has no dedicated step identity)
// that is none of the shared pipeline identities, no CI identity and no other
// repository's release identity; admission refuses those, so storing one
// would only create a grant that can never authorize a run.
func (config Config) validateRuntimeReleaseWIFGrant(repo string, grant ReleaseWIFRepository) error {
	if grant.ServiceAccountName == "" {
		return errors.New("runtime grant requires the repository's release service_account_name")
	}
	switch grant.ServiceAccountName {
	case config.PipelineServiceAccount, config.CredentialedServiceAccount, config.CISecretsServiceAccount, config.ExecutorServiceAccount:
		return fmt.Errorf("runtime grant for %s cannot use shared ServiceAccount %q", repo, grant.ServiceAccountName)
	}
	for other, identity := range config.PerRepoIdentities {
		if other != repo && identity.ServiceAccountName == grant.ServiceAccountName {
			return fmt.Errorf("runtime grant for %s cannot use %s's release ServiceAccount %q", repo, other, grant.ServiceAccountName)
		}
	}
	for other, identity := range config.PerRepoCIIdentities {
		if identity.ServiceAccountName == grant.ServiceAccountName {
			return fmt.Errorf("runtime grant for %s cannot use %s's CI ServiceAccount %q", repo, other, grant.ServiceAccountName)
		}
	}
	if err := config.ReleaseWIF.Validate(); err != nil {
		return err
	}
	return config.validateReleaseWIFStepAccounts()
}

// withRequestReleaseWIF applies an admission's pinned runtime capability map.
// Config is a value; the boot snapshot is never mutated.
func (config Config) withRequestReleaseWIF(request Request) Config {
	if request.ReleaseWIF != nil {
		config.ReleaseWIF = request.ReleaseWIF
	}
	return config
}
