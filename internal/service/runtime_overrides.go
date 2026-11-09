package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// Runtime administration (#1497): server-owned overrides layered on the Helm
// or flag boot defaults. Overrides live only in the audited store, so a Helm
// upgrade never shares an object with them and cannot conflict.

// ReleaseWIFOverrideStore persists audited runtime release WIF decisions.
type ReleaseWIFOverrideStore interface {
	ReleaseWIFOverrides(context.Context) ([]model.ReleaseWIFOverride, error)
	SetReleaseWIFOverride(context.Context, string, model.ReleaseWIFOverrideSpec) (model.ReleaseWIFOverride, error)
	ClearReleaseWIFOverride(context.Context, string, model.ReleaseWIFOverrideSpec) (model.ReleaseWIFOverride, error)
}

// ReleaseWIFPolicy merges overrides with the boot capability map. Preview
// returns the effective map and inactive grants a candidate set would
// publish without publishing it.
type ReleaseWIFPolicy interface {
	Preview([]model.ReleaseWIFOverride) (map[string]json.RawMessage, []model.ReleaseWIFOverrideStatus)
	Apply([]model.ReleaseWIFOverride) []model.ReleaseWIFOverrideStatus
	State() model.ReleaseWIFState
}

// AdmissionCapacityOverrideStore persists the audited runtime ceiling.
type AdmissionCapacityOverrideStore interface {
	AdmissionCapacityOverride(context.Context) (*model.AdmissionCapacityOverride, error)
	SetAdmissionCapacityOverride(context.Context, string, int, string) (*model.AdmissionCapacityOverride, error)
}

// RuntimeCapacityController validates and applies the runtime ceiling.
type RuntimeCapacityController interface {
	ValidateRuntimeCapacity(int) error
	ApplyRuntimeCapacity(context.Context) model.AdmissionCapacityConfig
}

type qualifiedRepositoryNamer interface {
	QualifiedRepoName(context.Context, int64) (string, error)
}

func requireRuntimeAdmin(actor api.Actor, tool string) error {
	if !actor.Admin {
		return refuse(ErrForbidden, "%s requires an admin uplink", tool)
	}
	return requireUnscoped(actor, tool)
}

func runtimeOverrideReason(reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 512 {
		return refuse(ErrInvalidInput, "a 1..512 byte reason is required")
	}
	return nil
}

// ReloadReleaseWIF publishes the stored overrides. Startup and
// release_wif_list call it; an invalid stored grant stays inactive and is
// reported.
func ReloadReleaseWIF(ctx context.Context, overrides ReleaseWIFOverrideStore, policy ReleaseWIFPolicy) ([]model.ReleaseWIFOverrideStatus, error) {
	values, err := overrides.ReleaseWIFOverrides(ctx)
	if err != nil {
		return nil, err
	}
	return policy.Apply(values), nil
}

func refuseInactiveReleaseWIF(inactive []model.ReleaseWIFOverrideStatus) error {
	if len(inactive) == 0 {
		return nil
	}
	return refuse(ErrInvalidInput, "release WIF override for %s: %s", inactive[0].Repo, inactive[0].Error)
}

func (service *API) releaseWIFTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	if err := requireRuntimeAdmin(actor, name); err != nil {
		return nil, err
	}
	if service.releaseWIFOverrides == nil || service.releaseWIF == nil {
		return nil, refuse(ErrUnavailable, "runtime release WIF administration")
	}
	if name == "release_wif_list" {
		if err := decodeTool(raw, &struct{}{}); err != nil {
			return nil, err
		}
		// Listing re-syncs the admission map with the store, so it also
		// repairs any drift between a stored change and the published map.
		service.releaseWIFMu.Lock()
		defer service.releaseWIFMu.Unlock()
		overrides, err := service.releaseWIFOverrides.ReleaseWIFOverrides(ctx)
		if err != nil {
			return nil, err
		}
		service.releaseWIF.Apply(overrides)
		return service.releaseWIFView(overrides), nil
	}
	var args struct {
		Repo               string            `json:"repo"`
		ServiceAccountName string            `json:"service_account_name"`
		Templates          map[string]string `json:"templates"`
		Reason             string            `json:"reason"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if err := runtimeOverrideReason(args.Reason); err != nil {
		return nil, err
	}
	if name != "release_wif_grant" && (args.ServiceAccountName != "" || args.Templates != nil) {
		return nil, refuse(ErrInvalidInput, "%s takes only repo and reason", name)
	}
	repo, registered, err := service.releaseWIFRepositoryKey(ctx, args.Repo)
	if err != nil {
		return nil, err
	}
	// One change at a time: the candidate check, the write and the publish
	// must see the same override set, or two valid grants could combine into
	// a conflict.
	service.releaseWIFMu.Lock()
	defer service.releaseWIFMu.Unlock()
	current, err := service.releaseWIFOverrides.ReleaseWIFOverrides(ctx)
	if err != nil {
		return nil, err
	}
	others := make([]model.ReleaseWIFOverride, 0, len(current)+1)
	var existing *model.ReleaseWIFOverride
	for i := range current {
		if current[i].Repo == repo {
			existing = &current[i]
			continue
		}
		others = append(others, current[i])
	}
	state := service.releaseWIF.State()
	spec := model.ReleaseWIFOverrideSpec{Repo: repo, Reason: args.Reason, PreviousEffective: state.Effective[repo]}
	// next is the complete override set once the change is stored.
	next := others
	switch name {
	case "release_wif_grant":
		if registered == nil {
			return nil, fmt.Errorf("%w: release WIF grants require a registered repository: %s", store.ErrNotFound, args.Repo)
		}
		if err := refuseArchivedGrant(*registered, name, repo); err != nil {
			return nil, err
		}
		spec.Grant, err = json.Marshal(map[string]any{"service_account_name": args.ServiceAccountName, "templates": args.Templates})
		if err != nil {
			return nil, err
		}
		next = append(next, model.ReleaseWIFOverride{Repo: repo, Grant: spec.Grant})
	case "release_wif_revoke":
		if _, active := state.Effective[repo]; !active && (existing == nil || existing.Revoked) {
			return nil, refuse(store.ErrInvalidState, "%s has no active release WIF entry", repo)
		}
		spec.Revoked = true
		next = append(next, model.ReleaseWIFOverride{Repo: repo, Revoked: true})
	case "release_wif_reset":
		if existing == nil {
			return nil, refuse(store.ErrInvalidState, "%s has no runtime release WIF override", repo)
		}
	default:
		return nil, fmt.Errorf("%w: unknown release WIF tool %s", ErrInvalidInput, name)
	}
	effective, inactive := service.releaseWIF.Preview(next)
	// A revoke only narrows authority, so an unrelated inactive grant never
	// blocks it. A grant, or restoring a boot entry, must not conflict.
	if name != "release_wif_revoke" {
		if err := refuseInactiveReleaseWIF(inactive); err != nil {
			return nil, err
		}
	}
	spec.Effective = effective[repo]
	var stored model.ReleaseWIFOverride
	if name == "release_wif_reset" {
		_, err = service.releaseWIFOverrides.ClearReleaseWIFOverride(ctx, actor.Identity, spec)
	} else {
		stored, err = service.releaseWIFOverrides.SetReleaseWIFOverride(ctx, actor.Identity, spec)
	}
	if err != nil {
		return nil, err
	}
	// Publish the validated set the store just committed, without another
	// read: a cancelled or failing request must not leave admission on the
	// old map after the change is stored and audited.
	if name != "release_wif_reset" {
		next[len(next)-1] = stored
	}
	slices.SortFunc(next, func(a, b model.ReleaseWIFOverride) int { return strings.Compare(a.Repo, b.Repo) })
	service.releaseWIF.Apply(next)
	return service.releaseWIFView(next), nil
}

func (service *API) releaseWIFView(overrides []model.ReleaseWIFOverride) map[string]any {
	if overrides == nil {
		overrides = []model.ReleaseWIFOverride{}
	}
	state := service.releaseWIF.State()
	return map[string]any{"enabled": state.Enabled, "roles": state.Roles, "boot": state.Boot, "overrides": overrides, "effective": state.Effective, "inactive": state.Inactive}
}

// A registered repository always uses its canonical "upstream/org/repo" key.
// An unregistered exact key remains revocable, so a retired repository's
// boot entry can be removed without a Helm change.
func (service *API) releaseWIFRepositoryKey(ctx context.Context, name string) (string, *model.Repository, error) {
	if strings.TrimSpace(name) == "" || len(name) > 512 {
		return "", nil, refuse(ErrInvalidInput, "repo is required")
	}
	if service.repositories == nil {
		return "", nil, refuse(ErrUnavailable, "repositories")
	}
	repository, err := service.repositories.RepositoryByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		if parts := strings.Split(name, "/"); len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return "", nil, fmt.Errorf("%w: unregistered repositories must be named upstream/org/repo", ErrInvalidInput)
		}
		return name, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	namer, ok := service.repositories.(qualifiedRepositoryNamer)
	if !ok {
		return "", nil, refuse(ErrUnavailable, "repository canonical names")
	}
	key, err := namer.QualifiedRepoName(ctx, repository.ID)
	if err != nil {
		return "", nil, err
	}
	return key, &repository, nil
}

func (service *API) admissionCapacityConfig(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if err := requireRuntimeAdmin(actor, "admission_capacity_config"); err != nil {
		return nil, err
	}
	if service.capacityOverrides == nil || service.runtimeCapacity == nil {
		return nil, refuse(ErrUnavailable, "runtime admission capacity")
	}
	var args struct {
		MaxConcurrentJobs *int   `json:"max_concurrent_jobs"`
		Clear             bool   `json:"clear"`
		Reason            string `json:"reason"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if err := runtimeOverrideReason(args.Reason); err != nil {
		return nil, err
	}
	if (args.MaxConcurrentJobs == nil) == !args.Clear {
		return nil, refuse(ErrInvalidInput, "pass exactly one of max_concurrent_jobs (0..64; 0 is no ceiling) or clear")
	}
	jobs := 0
	if args.MaxConcurrentJobs != nil {
		jobs = *args.MaxConcurrentJobs
		if err := service.runtimeCapacity.ValidateRuntimeCapacity(jobs); err != nil {
			return nil, refuse(ErrInvalidInput, "%v", err)
		}
	}
	override, err := service.capacityOverrides.SetAdmissionCapacityOverride(ctx, actor.Identity, jobs, args.Reason)
	if err != nil {
		return nil, err
	}
	// The override is committed: reload it even if the caller has gone, or
	// admission keeps the old ceiling until the next periodic reload.
	configuration := service.runtimeCapacity.ApplyRuntimeCapacity(context.WithoutCancel(ctx))
	return map[string]any{"override": override, "configuration": configuration}, nil
}
