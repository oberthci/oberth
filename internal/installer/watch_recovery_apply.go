package installer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	sigsyaml "sigs.k8s.io/yaml"
)

const originalWatchPlanSHA = "11cdaed77f997adc5393ec79517c55f7dba2fc4a282150c088ac99c37eefb203"
const originalWatchReceiptSHA = "a9c74791b7d763ecb4e2491af18f69e5009454760ba6e585afe377712f13791d"

func recoveryOrigin(p watchRecoveryPlan) error {
	if p.Origin == nil || p.Origin.Plan.SHA256 != originalWatchPlanSHA || p.Origin.Receipt.SHA256 != originalWatchReceiptSHA || len(p.Origin.Confirmed) != 4 {
		return errors.New("watch recovery original attempt binding differs")
	}
	raw, err := watchFile(p.Origin.Plan, 262144)
	if err != nil {
		return err
	}
	var old watchAdoptionPlan
	if !exactWatchFields(raw) || strictWatchDecode(raw, &old) != nil || old.Schema != watchAdoptionSchema || old.Namespace != p.Namespace || canonicalChartVersion(old.ChartVersion) != "v0.16.25" || old.ReleaseRevision != 71 || len(old.Objects) != 4 {
		return errors.New("original watch plan scope differs")
	}
	raw, err = watchFile(p.Origin.Receipt, 65536)
	if err != nil {
		return err
	}
	var receipt struct {
		Phase      string         `json:"phase"`
		State      string         `json:"state"`
		Source     string         `json:"source"`
		Version    string         `json:"version"`
		Exit       int            `json:"exit"`
		Plan       string         `json:"public_plan_sha256"`
		Confirmed  []watchAdopted `json:"confirmed_metadata_commits"`
		Projection bool           `json:"public_receipt_projection_valid"`
		Joined     bool           `json:"all_owned_children_joined"`
	}
	// The exact hash identifies the previously reviewed immutable public receipt.
	if !uniqueWatchJSON(raw) || json.Unmarshal(raw, &receipt) != nil || receipt.Phase != "execute" || receipt.State != "outcome-uncertain-no-retry" || receipt.Source != "b412c78a6b1e6e50d967435dbb608a442c1fd11c" || receipt.Version != "v0.16.25" || receipt.Exit != 1 || receipt.Plan != originalWatchPlanSHA || !receipt.Projection || !receipt.Joined || len(receipt.Confirmed) != 4 {
		return errors.New("original watch execution receipt differs")
	}
	for _, o := range p.Objects {
		matches := 0
		for _, row := range receipt.Confirmed {
			if row.Kind == o.Kind && row.Name == o.Name && row.UID == o.UID {
				for _, bound := range p.Origin.Confirmed {
					if row == bound {
						matches++
					}
				}
			}
		}
		for _, original := range old.Objects {
			if original.Kind == o.Kind && original.Name == o.Name && original.UID == o.UID {
				matches++
			}
		}
		if matches != 2 {
			return errors.New("watch recovery is not bound to original confirmed UIDs")
		}
	}
	return nil
}

func requireRecoveryGuard(ctx context.Context, deps Deps, p watchRecoveryPlan) error {
	if deps.KubeClient == nil || !watchRecoveryDeadline(p) || deps.ContextName != p.Context {
		return errors.New("watch recovery context or deadline differs")
	}
	ns, err := deps.KubeClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil || string(ns.UID) != p.ClusterUID {
		return errors.New("watch recovery cluster UID differs")
	}
	v, err := deps.KubeClient.Discovery().ServerVersion()
	if err != nil || v.GitVersion != p.ServerVersion {
		return errors.New("watch recovery exact API version differs")
	}
	raw, err := deps.RunHelm(ctx, []string{"history", "oberth", "-n", p.Namespace, "--max", "2", "-o", "json"})
	defer clear(raw)
	var rows []helmRelease
	if err != nil || !uniqueWatchJSON(raw) || json.Unmarshal(raw, &rows) != nil || len(rows) != 2 {
		return errors.New("cannot establish exact latest Helm history")
	}
	latest, previous := rows[1], rows[0]
	revision := func(r helmRelease) int64 {
		n, e := strconv.ParseInt(strings.Trim(string(r.Revision), `"`), 10, 64)
		if e != nil {
			return -1
		}
		return n
	}
	if revision(latest) != p.Release.Revision || latest.Status != p.Release.Status || latest.Chart != p.Release.Chart || revision(previous) != p.Release.PreviousRevision || previous.Status != "deployed" {
		return errors.New("watch recovery latest failed history moved")
	}
	client := deps.KubeClient.CoreV1().RESTClient()
	if client == nil {
		return errors.New("watch recovery requires real metadata API observation")
	}
	stream, err := client.Get().Namespace(p.Namespace).Resource("secrets").Name(fmt.Sprintf("sh.helm.release.v1.oberth.v%d", p.Release.Revision)).SetHeader("Accept", "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1").Stream(ctx)
	if err != nil {
		return errors.New("cannot observe failed Helm record metadata")
	}
	body, err := io.ReadAll(io.LimitReader(stream, 65537))
	_ = stream.Close()
	defer clear(body)
	var record metav1.PartialObjectMetadata
	if err != nil || len(body) > 65536 || strictWatchDecode(body, &record) != nil || record.Kind != "PartialObjectMetadata" || record.APIVersion != "meta.k8s.io/v1" || string(record.UID) != p.Release.RecordUID || record.ResourceVersion != p.Release.RecordRV || record.Namespace != p.Namespace || record.Name != fmt.Sprintf("sh.helm.release.v1.oberth.v%d", p.Release.Revision) || record.Labels["name"] != "oberth" || record.Labels["owner"] != "helm" || record.Labels["status"] != p.Release.Status || record.Labels["version"] != strconv.FormatInt(p.Release.Revision, 10) || record.DeletionTimestamp != nil {
		return errors.New("failed Helm record identity or public state moved")
	}
	if !watchRecoveryDeadline(p) {
		return errors.New("watch recovery expired during observation")
	}
	return nil
}

func recoveryPreview(ctx context.Context, cfg Config, deps Deps, p watchRecoveryPlan) error {
	args := append(OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{}), "--dry-run=server", "--output=json", "--no-hooks", "--take-ownership")
	raw, err := deps.RunHelm(ctx, args)
	defer clear(raw)
	if err != nil || len(raw) > 8<<20 {
		return errors.New("watch recovery Helm preview failed")
	}
	var preview struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Version   int64  `json:"version"`
		Manifest  string `json:"manifest"`
		Chart     struct {
			Metadata struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"chart"`
	}
	if !uniqueWatchJSON(raw) || json.Unmarshal(raw, &preview) != nil || preview.Name != "oberth" || preview.Namespace != p.Namespace || preview.Version != p.Release.Revision+1 || canonicalChartVersion(preview.Chart.Metadata.Version) != canonicalChartVersion(p.ChartVersion) || preview.Chart.Metadata.Name != "oberth" {
		return errors.New("watch preview release identity differs")
	}
	r := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(preview.Manifest)))
	found := map[string]bool{}
	for {
		document, e := r.Read()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return errors.New("watch preview YAML invalid")
		}
		body, e := sigsyaml.YAMLToJSONStrict(document)
		if e != nil {
			return errors.New("duplicate or invalid watch preview YAML")
		}
		if len(bytes.TrimSpace(body)) == 0 || bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
			continue
		}
		var id struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		if json.Unmarshal(body, &id) != nil {
			return errors.New("watch preview object identity invalid")
		}
		if !watchObjectAllowed(id.Kind, id.Metadata.Name) {
			continue
		}
		key := id.Kind + "/" + id.Metadata.Name
		if found[key] {
			return errors.New("duplicate watch preview object")
		}
		found[key] = true
		for _, o := range p.Objects {
			if o.Kind != id.Kind || o.Name != id.Metadata.Name {
				continue
			}
			wantObj, e := recoveryTarget(o, false, p.Namespace)
			if e != nil {
				return e
			}
			got := emptyRecoveryObject(o.Kind)
			if strictWatchDecode(body, got) != nil {
				return errors.New("watch preview typed object invalid")
			}
			v, e := watchSpec(got)
			if e != nil {
				return e
			}
			if v.meta.GetAnnotations() == nil {
				v.meta.SetAnnotations(map[string]string{})
			}
			annotations := v.meta.GetAnnotations()
			// Helm injects these release annotations into its actual target after
			// chart rendering. Reject an explicit foreign template value.
			for k, want := range map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": p.Namespace} {
				if old, ok := annotations[k]; ok && old != want {
					return errors.New("foreign watch preview ownership")
				}
				annotations[k] = want
			}
			v.meta.SetAnnotations(annotations)
			w, _ := watchSpec(wantObj)
			if v.meta.GetNamespace() != p.Namespace || !sameWatchJSON(v.spec, w.spec) || !sameRecoveryMetadata(v.meta, watchPublicMetadata{w.meta.GetLabels(), w.meta.GetAnnotations()}) {
				return errors.New("watch recovery render differs from reviewed target")
			}
		}
	}
	if len(found) != 4 {
		return errors.New("watch recovery preview inventory differs")
	}
	return nil
}

// AI-CONTRACT: The v2 path is only a fresh, expiring forward repair of the
// original failed72 attempt. It never widens manager transfer or writes specs.
func executeWatchRecovery(ctx context.Context, cfg Config, deps Deps, dryRun bool) ([]watchAdopted, error) {
	if cfg.watchRecovery == nil {
		return nil, errors.New("watch signed recovery plan absent")
	}
	if err := recoveryOrigin(*cfg.watchRecovery); err != nil {
		return nil, err
	}
	return applyWatchRecovery(ctx, cfg, deps, dryRun, func(p watchRecoveryPlan) error { return requireRecoveryGuard(ctx, deps, p) })
}

// The effect engine is also used by the isolated real-API regression with its
// owned synthetic release. Production always supplies requireRecoveryGuard.
func applyWatchRecovery(ctx context.Context, cfg Config, deps Deps, dryRun bool, guard func(watchRecoveryPlan) error) ([]watchAdopted, error) {
	if cfg.watchRecovery == nil || cfg.watchChart == "" || deps.Output == nil {
		return nil, errors.New("watch recovery requires prepared signed target and public receipt output")
	}
	p := *cfg.watchRecovery
	p.Objects = append([]watchRecoveryObject(nil), p.Objects...)
	p.ready = false
	if err := guard(p); err != nil {
		return nil, err
	}
	observed, err := recoveryObserved(ctx, deps, p)
	if err != nil {
		return nil, err
	}
	if err = recoveryPreview(ctx, cfg, deps, p); err != nil {
		return nil, err
	}
	resume := p.Mode == "post-handoff-resume"
	if err = recoverySSAWithGuard(ctx, deps, p, !resume, func() error { return guard(p) }); err != nil {
		return nil, err
	}
	if resume {
		rows, err := watchFile(p.Resume.HandoffReceipt, 65536)
		if err != nil {
			return nil, err
		}
		var receipt []watchAdopted
		if !uniqueWatchJSON(rows) || strictWatchDecode(rows, &receipt) != nil || len(receipt) != 1 {
			return nil, errors.New("confirmed handoff receipt differs")
		}
		if dryRun {
			return nil, nil
		}
		if _, err = recoveryObserved(ctx, deps, p); err != nil {
			return receipt, err
		}
		if err = guard(p); err != nil {
			return receipt, err
		}
		encoded, _ := json.Marshal(receipt)
		if _, err = fmt.Fprintf(deps.Output, "Watch connector confirmed handoff resumed: %s\n", encoded); err != nil {
			return receipt, errors.New("watch resume receipt output failed; stop and retain state")
		}
		p.ready = true
		*cfg.watchRecovery = p
		return receipt, nil
	}
	index := -1
	for i, o := range p.Objects {
		if o.Kind == "Deployment" {
			index = i
		}
	}
	if index < 0 {
		return nil, errors.New("watch Deployment missing")
	}
	ownerIndex, next, err := watchArgsHandoff(observed[index].meta.GetManagedFields())
	if err != nil {
		return nil, err
	}
	body, err := watchMetadataCAS(observed[index], ownerIndex)
	if err != nil {
		return nil, err
	}
	if err = guard(p); err != nil {
		return nil, err
	}
	reply, err := patchRecoveryObject(ctx, deps, p.Namespace, "Deployment", p.Objects[index].Name, types.JSONPatchType, body, metav1.PatchOptions{FieldManager: "oberth-watch-adoption", DryRun: []string{metav1.DryRunAll}, FieldValidation: "Strict"})
	if err != nil || !validHandoffReply(reply, observed[index], p.Objects[index], p.Namespace, next, false) {
		return nil, errors.New("watch metadata handoff dry-run differs from reviewed subtraction")
	}
	if dryRun {
		return nil, nil
	}
	if _, err = recoveryObserved(ctx, deps, p); err != nil {
		return nil, err
	}
	if err = guard(p); err != nil {
		return nil, err
	}
	reply, err = patchRecoveryObject(ctx, deps, p.Namespace, "Deployment", p.Objects[index].Name, types.JSONPatchType, body, metav1.PatchOptions{FieldManager: "oberth-watch-adoption", FieldValidation: "Strict"})
	if err != nil || !validHandoffReply(reply, observed[index], p.Objects[index], p.Namespace, next, true) {
		return nil, errors.New("watch handoff outcome uncertain for Deployment/cloudflared-watch-oberth-v2; retain state, no automatic retry or rollback")
	}
	v, _ := watchSpec(reply)
	o := &p.Objects[index]
	o.ResourceVersion = v.meta.GetResourceVersion()
	o.ManagedFields = v.meta.GetManagedFields()
	receipt := []watchAdopted{{Kind: o.Kind, Name: o.Name, UID: o.UID, ResourceVersion: o.ResourceVersion}}
	encoded, _ := json.Marshal(receipt)
	if _, err = fmt.Fprintf(deps.Output, "Watch connector legacy args ownership relinquished: %s\n", encoded); err != nil {
		return receipt, errors.New("watch handoff confirmed but public receipt output failed; stop and retain state")
	}
	*cfg.watchRecovery = p
	if _, err = recoveryObserved(ctx, deps, p); err != nil {
		return receipt, err
	}
	if err = recoverySSAWithGuard(ctx, deps, p, false, func() error { return guard(p) }); err != nil {
		return receipt, err
	}
	if _, err = recoveryObserved(ctx, deps, p); err != nil {
		return receipt, err
	}
	if err = guard(p); err != nil {
		return receipt, err
	}
	p.ready = true
	*cfg.watchRecovery = p
	return receipt, nil
}

func validHandoffReply(reply any, before watchObserved, o watchRecoveryObject, ns string, next []metav1.ManagedFieldsEntry, committed bool) bool {
	v, e := watchSpec(reply)
	if e != nil {
		return false
	}
	rv := v.meta.GetResourceVersion()
	if rv == "" || committed && rv == o.ResourceVersion || !committed && rv != o.ResourceVersion {
		return false
	}
	got, e := watchCompleteMetadata(reply)
	want, f := watchCompleteMetadata(before.meta)
	want.ResourceVersion, want.ManagedFields = rv, next
	return e == nil && f == nil && sameWatchCompleteMetadata(got, want) && v.meta.GetNamespace() == ns && v.meta.GetName() == o.Name && string(v.meta.GetUID()) == o.UID && v.meta.GetGeneration() == o.Generation && v.meta.GetDeletionTimestamp() == nil && sameWatchJSON(v.spec, o.Spec)
}
func requireWatchRecoveryBeforeHelm(ctx context.Context, cfg Config, deps Deps) error {
	if cfg.watchRecovery == nil {
		return nil
	}
	if !cfg.watchRecovery.ready {
		return errors.New("watch recovery real SSA qualification incomplete")
	}
	if _, err := recoveryObserved(ctx, deps, *cfg.watchRecovery); err != nil {
		return err
	}
	return requireRecoveryGuard(ctx, deps, *cfg.watchRecovery)
}
