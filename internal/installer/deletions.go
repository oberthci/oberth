package installer

import (
	"context"
	"fmt"
	"io"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// preflight renders with the native upgrade against a private history snapshot.
// Pending status changes in this snapshot never reach cluster release storage.
func (l ChartLibrary) preflight(ctx context.Context, config *action.Configuration, r ChartRequest) error {
	if err := checkApplyHistory(ctx, config, r); err != nil {
		return err
	}
	if r.Revision == 0 {
		return nil
	}
	snapshot, err := snapshotHistory(ctx, config, r)
	if err != nil {
		return err
	}
	lists := &upgradeLists{Interface: config.KubeClient}
	snapshot.KubeClient = lists
	if _, err = l.upgradeAction(snapshot, r, true).RunWithContext(ctx, "oberth", r.Chart, r.Values); err != nil {
		return err
	}
	if !lists.originalSeen {
		return described("target", nil, "The native preview did not expose its predecessor resources.")
	}
	return checkDeletions(ctx, r, lists.original.Difference(lists.target))
}

// C9 validates every target. In the pinned native action's read-only run,
// Build(false) supplies its original list; Build(true) supplies its target.
// Capture those native lists rather than duplicating its baseline selection.
type upgradeLists struct {
	kube.Interface
	original, target kube.ResourceList
	originalSeen     bool
}

func (l *upgradeLists) Build(reader io.Reader, validate bool) (kube.ResourceList, error) {
	infos, err := l.Interface.Build(reader, validate)
	if validate {
		l.target = infos
	} else {
		l.original, l.originalSeen = infos, true
	}
	return infos, err
}

func snapshotHistory(ctx context.Context, config *action.Configuration, r ChartRequest) (*action.Configuration, error) {
	records, err := config.Releases.History("oberth")
	if err != nil {
		return nil, err
	}
	snapshot := *config
	snapshot.Releases = storage.Init(driver.NewMemory())
	for _, record := range records {
		if _, err := readRevision(record, r.Namespace); err != nil {
			return nil, err
		}
		copied, info := *record, *record.Info
		copied.Info = &info
		if unfinished(string(info.Status)) {
			info.Status = repairStatus(info.Status)
		}
		if err = snapshot.Releases.Create(&copied); err != nil {
			return nil, err
		}
	}
	if err = checkApplyHistory(ctx, &snapshot, r); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func checkDeletions(ctx context.Context, r ChartRequest, removed kube.ResourceList) error {
	values, err := inputs(r.Chart, r.Values, r.Namespace)
	if err != nil {
		return err
	}
	pipeline, _ := values["pipeline"].(map[string]any)
	for _, info := range removed {
		identity := fmt.Sprintf("%s %s/%s", info.Mapping.GroupVersionKind.Kind, info.Namespace, info.Name)
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := info.Get(); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return described("history-delete", err, identity)
		}
		if info.Namespaced() && info.Namespace != r.Namespace && info.Namespace != pipeline["namespace"] {
			return described("history-delete", nil, identity)
		}
		live, err := meta.Accessor(info.Object)
		if err != nil || !ownedByRelease(live, r.Namespace) {
			return described("history-delete", err, identity)
		}
	}
	return nil
}

func checkApplyHistory(ctx context.Context, config *action.Configuration, r ChartRequest) error {
	history, err := readHistory(ctx, config, r.Namespace)
	if err != nil {
		return err
	}
	return r.CheckHistory(history)
}

func ownedByRelease(object metav1.Object, namespace string) bool {
	return object.GetLabels()["app.kubernetes.io/managed-by"] == "Helm" && object.GetAnnotations()["meta.helm.sh/release-name"] == "oberth" && object.GetAnnotations()["meta.helm.sh/release-namespace"] == namespace
}

// Recheck the native action's actual delete set before resource changes, too.
// The read-only preflight above protects history bookkeeping from static refusals.
type deletionGuard struct {
	kube.Interface
	ctx     context.Context
	request ChartRequest
}

func (g deletionGuard) Update(original, target kube.ResourceList, force bool) (*kube.Result, error) {
	if err := checkDeletions(g.ctx, g.request, original.Difference(target)); err != nil {
		return &kube.Result{}, err
	}
	return g.Interface.Update(original, target, force)
}
