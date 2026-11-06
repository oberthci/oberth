package argojob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Both direct phases use the same canonical run binding and authored graph.
func (p *ExecutionPlacement) selectPlacementFragments(r Request) error {
	d := p.document
	if len(r.Fragments) != len(d.Fragments) {
		return errors.New("argojob: fragment inventory differs from placement")
	}
	for _, expected := range d.Fragments {
		key, err := argoworkflow.ParseFragmentRef(expected.Key)
		if err != nil {
			return err
		}
		fragment, present := r.Fragments[key]
		sum := sha256.Sum256(fragment.Source)
		if !present || fragment.Key != key || fragment.SHA != expected.Commit || hex.EncodeToString(sum[:]) != expected.Digest || (fragment.Digest != "" && fragment.Digest != expected.Digest) {
			return errors.New("argojob: authentic fragment content differs from placement")
		}
	}
	resolved, err := argoworkflow.ResolveRunSource(r.Source, r.Fragments, argoworkflow.RunSourceBinding{RunID: r.RunID, RepositoryID: r.RepositoryID, SHA: r.SHA, File: r.PipelineFile, Trigger: r.effectiveRunTrigger(), ScheduleEntry: r.ScheduleEntry})
	if err != nil {
		return err
	}
	if resolved.GraphIdentity() != d.GraphDigest || placementHash(resolved.Lock()) != d.FragmentLockDigest {
		return errors.New("argojob: resolved graph or fragment lock differs from placement")
	}
	return nil
}
