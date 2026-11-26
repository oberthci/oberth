package argojob

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

type resumeExporter func(context.Context, sourcecontract.Binding) (io.ReadCloser, error)

func mainResumeBinding(view admissioncontract.PreparedView) (sourcecontract.Binding, error) {
	for _, b := range view.SourceBindings {
		if b.Descriptor.Kind == sourcecontract.Main && b.Path == "/work/src" {
			return b, nil
		}
	}
	return sourcecontract.Binding{}, resumeRefusal(ReasonResumeUnsafe, "held step needs a main checkout at /work/src")
}

// PrepareResumeCheckout seals the transfer digest before the atomic handoff.
// Stage is C6's frozen admission export, including HEAD, index, own ref,
// default head and tags. Pins are never staged or modified here.
func (e *Executor) PrepareResumeCheckout(ctx context.Context, old stateport.Run, fresh admissioncontract.PreparedView, plan nativebinding.ResumePlan) (ResumePlan, error) {
	if plan.InPod == nil {
		return plan, fmt.Errorf("missing held attempt")
	}
	exporter := e.config.ResumeExport
	if exporter == nil {
		return plan, resumeRefusal(ReasonResumeStart, "checkout export unavailable")
	}
	previous, err := mainResumeBinding(old.Admission)
	if err != nil {
		return plan, err
	}
	fix, err := mainResumeBinding(fresh)
	if err != nil {
		return plan, err
	}
	before, err := resumeInventory(ctx, exporter, previous)
	if err != nil {
		return plan, err
	}
	after, err := resumeInventory(ctx, exporter, fix)
	if err != nil {
		return plan, err
	}
	removed := []string{}
	for p := range before {
		if !after[p] {
			removed = append(removed, p)
		}
	}
	slices.Sort(removed)
	plan.InPod.Source, plan.InPod.Removed = fix, removed
	plan.InPod.RunID = fresh.RunID
	reader, err := resumeExport(ctx, exporter, *plan.InPod)
	if err != nil {
		return plan, err
	}
	defer reader.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(reader, maxRerunArchive+1))
	if err != nil {
		return plan, err
	}
	if n > maxRerunArchive {
		return plan, resumeRefusal(ReasonResumeUnsafe, "snapshot exceeds transfer limit")
	}
	plan.InPod.Digest = hex.EncodeToString(hash.Sum(nil))
	return plan, nil
}

func resumeInventory(ctx context.Context, exporter resumeExporter, binding sourcecontract.Binding) (map[string]bool, error) {
	stage, err := exporter(ctx, binding)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	gz, err := gzip.NewReader(stage)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(io.LimitReader(&resumeContextReader{ctx, gz}, maxRerunTree+(16<<20)))
	paths := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return paths, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Name != ".git" && !strings.HasPrefix(h.Name, ".git/") {
			if !snapshotPath(h.Name) || len(paths) >= 100000 {
				return nil, fmt.Errorf("invalid checkout inventory")
			}
			paths[h.Name] = true
		}
	}
}

type resumeExportReader struct {
	io.Reader
	io.Closer
}

func resumeExport(ctx context.Context, exporter resumeExporter, held nativebinding.ResumeHold) (io.ReadCloser, error) {
	var manifest bytes.Buffer
	if err := writeHoldFrame(&manifest, rerunManifest{RunID: held.RunID, Commit: held.Source.Revision.Commit, Removed: held.Removed}); err != nil {
		return nil, err
	}
	if manifest.Len() > maxHoldFrame {
		return nil, fmt.Errorf("snapshot manifest too large")
	}
	stage, err := exporter(ctx, held.Source)
	if err != nil {
		return nil, err
	}
	return &resumeExportReader{Reader: io.MultiReader(&manifest, stage), Closer: stage}, nil
}
