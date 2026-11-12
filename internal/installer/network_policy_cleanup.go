package installer

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Bounded probes publish an expiry including cleanup and clock-skew grace.
// Unknown lifetime metadata never grants deletion authority.
func reconcilePolicyProbes(ctx context.Context, deps Deps, timing policyProbeTiming) (string, error) {
	namespaces := deps.KubeClient.CoreV1().Namespaces()
	list, err := namespaces.List(ctx, metav1.ListOptions{LabelSelector: policyProbeLabel})
	if err != nil {
		return "", err
	}
	var active []string
	for _, ns := range list.Items {
		token := ns.Labels[policyProbeLabel]
		decoded, err := hex.DecodeString(token)
		if err != nil || len(decoded) != 8 || ns.Name != "oberth-policy-probe-"+token {
			active = append(active, ns.Name+" (ownership unrecognized; untouched)")
			continue
		}
		expiry, err := time.Parse(time.RFC3339, ns.Annotations[policyProbeExpiry])
		if err != nil || ns.UID == "" {
			return "Probe lifetime/identity is unverified for " + ns.Name + "; untouched.", errors.New("owned probe lifetime or UID unverified")
		}

		if time.Now().Before(expiry) {
			active = append(active, ns.Name)
			continue
		}
		if err := cleanupObservedPolicyProbe(ctx, deps, ns.Name, token, ns.UID, true, timing.interval); err != nil {
			return "Prior owned probe namespace " + ns.Name + " cleanup is unverified.", err
		}
	}
	if len(active) == 0 {
		return "", nil
	}
	return "Recent or active owned probes preserved until their expiry: " + strings.Join(active, ","), nil
}

func cleanupObservedPolicyProbe(ctx context.Context, deps Deps, name, token string, authorizedUID types.UID, expired bool, interval time.Duration) error {
	namespaces := deps.KubeClient.CoreV1().Namespaces()
	ns, err := namespaces.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if authorizedUID == "" || ns.UID != authorizedUID || ns.Labels[policyProbeLabel] != token || name != "oberth-policy-probe-"+token {
		return errors.New("probe ownership changed")
	}
	if expired {
		expiry, err := time.Parse(time.RFC3339, ns.Annotations[policyProbeExpiry])
		if err != nil || time.Now().Before(expiry) {
			return errors.New("probe lifetime changed; namespace preserved")
		}
	}
	uid := authorizedUID
	preconditions := &metav1.Preconditions{UID: &uid}
	if ns.ResourceVersion != "" {
		version := ns.ResourceVersion
		preconditions.ResourceVersion = &version
	}
	if err := namespaces.Delete(ctx, name, metav1.DeleteOptions{Preconditions: preconditions}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return pollPolicyProbe(ctx, interval, func(ctx context.Context) (bool, error) {
		current, err := namespaces.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err == nil && current.UID != authorizedUID {
			return false, errors.New("namespace replaced during cleanup; replacement preserved")
		}
		return false, err
	})
}
