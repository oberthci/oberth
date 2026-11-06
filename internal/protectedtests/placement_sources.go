package protectedtests

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// PlacementFragment pins authentic input, independently of current tag routing.
type PlacementFragment struct {
	Key    string `json:"key"`
	Commit string `json:"commit"`
	Digest string `json:"digest"`
}

type PlacementFragmentSource struct {
	PlacementFragment
	Source []byte `json:"source"`
	// Preserve the optional caller field used by the existing submission identity.
	InputDigest string `json:"input_digest,omitempty"`
}

// PlacementSources is immutable reconstruction material, never current policy.
type PlacementSources struct {
	Source    []byte                    `json:"source"`
	Fragments []PlacementFragmentSource `json:"fragments"`
}

// NewFragmentPlacementAdmission binds original graph bytes in the same atomic
// admission as runtime and placement, within the existing storage bound.
func NewFragmentPlacementAdmission(placement, config []byte, runtime GenericAdmission, source []byte, fragments map[argoworkflow.FragmentKey]argoworkflow.Fragment) (PlacementAdmission, error) {
	snapshot := &PlacementSources{Source: bytes.Clone(source)}
	for key, fragment := range fragments {
		if fragment.Key != key || (fragment.Digest != "" && fragment.Digest != placementBodyDigest(fragment.Source)) {
			return PlacementAdmission{}, errors.New("invalid protected fragment source")
		}
		snapshot.Fragments = append(snapshot.Fragments, PlacementFragmentSource{PlacementFragment: PlacementFragment{key.String(), fragment.SHA, placementBodyDigest(fragment.Source)}, Source: bytes.Clone(fragment.Source), InputDigest: fragment.Digest})
	}
	sort.Slice(snapshot.Fragments, func(i, j int) bool { return snapshot.Fragments[i].Key < snapshot.Fragments[j].Key })
	a := PlacementAdmission{Version: 2, Placement: bytes.Clone(placement), Config: bytes.Clone(config), PlacementSHA256: placementBodyDigest(placement), ConfigSHA256: placementBodyDigest(config), RuntimeSHA256: runtime.Digest(), IntentSHA256: IntentDigest(runtime.Intent), Sources: snapshot}
	return a, a.Validate(runtime)
}

func (a PlacementAdmission) validateSources(s PlacementScope) error {
	if a.Sources == nil {
		if s.GraphDigest != "" || s.FragmentLockDigest != "" || len(s.Fragments) != 0 {
			return errors.New("protected fragment graph lacks retained sources")
		}
		return nil
	}
	body, err := json.Marshal(a)
	if err != nil || len(body) > 512<<10 {
		return errors.New("protected source custody exceeds admission bound")
	}
	snapshot := a.Sources
	if placementBodyDigest(snapshot.Source) != s.SourceDigest || len(snapshot.Fragments) > argoworkflow.MaxFragments || len(snapshot.Fragments) != len(s.Fragments) {
		return errors.New("protected source custody differs from placement")
	}
	expected := make(map[string]PlacementFragment, len(s.Fragments))
	for _, fragment := range s.Fragments {
		if _, present := expected[fragment.Key]; present {
			return errors.New("duplicate protected fragment")
		}
		expected[fragment.Key] = fragment
	}
	fragments := make(map[argoworkflow.FragmentKey]argoworkflow.Fragment, len(snapshot.Fragments))
	previous := ""
	for _, fragment := range snapshot.Fragments {
		key, err := argoworkflow.ParseFragmentRef(fragment.Key)
		if err != nil || key.String() != fragment.Key || fragment.Key <= previous || !argoworkflow.IsFullFragmentSHA(fragment.Commit) || expected[fragment.Key] != fragment.PlacementFragment || fragment.Digest != placementBodyDigest(fragment.Source) || (fragment.InputDigest != "" && fragment.InputDigest != fragment.Digest) {
			return errors.New("protected fragment bytes differ from placement")
		}
		previous = fragment.Key
		fragments[key] = argoworkflow.Fragment{Key: key, SHA: fragment.Commit, Digest: fragment.Digest, Source: fragment.Source}
	}
	resolved, err := argoworkflow.ResolveRunSource(snapshot.Source, fragments, argoworkflow.RunSourceBinding{RunID: s.RunID, RepositoryID: s.RepositoryID, SHA: s.SHA, File: s.PipelineFile, Trigger: argoworkflow.RunTriggerBranch})
	if err != nil {
		return err
	}
	lock, err := json.Marshal(resolved.Lock())
	if err != nil || s.GraphDigest != resolved.GraphIdentity() || s.FragmentLockDigest != placementBodyDigest(lock) {
		return errors.New("protected retained graph or lock differs from placement")
	}
	return nil
}

// RestoreSources revalidates the atomic admission before returning independent
// original bytes for observation. Callers must still check current execution authority.
func (a PlacementAdmission) RestoreSources(runtime GenericAdmission) ([]byte, map[argoworkflow.FragmentKey]argoworkflow.Fragment, error) {
	if err := a.Validate(runtime); err != nil {
		return nil, nil, err
	}
	if a.Sources == nil {
		return nil, nil, nil
	}
	fragments := make(map[argoworkflow.FragmentKey]argoworkflow.Fragment, len(a.Sources.Fragments))
	for _, fragment := range a.Sources.Fragments {
		key, err := argoworkflow.ParseFragmentRef(fragment.Key)
		if err != nil {
			return nil, nil, err
		}
		fragments[key] = argoworkflow.Fragment{Key: key, SHA: fragment.Commit, Digest: fragment.InputDigest, Source: bytes.Clone(fragment.Source)}
	}
	return bytes.Clone(a.Sources.Source), fragments, nil
}
