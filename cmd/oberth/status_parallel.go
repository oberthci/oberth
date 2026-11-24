package main

import (
	"context"
	"strconv"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const parallelOriginAnnotation = "oberth.ci/parallel-origin"
const settingsStatusTimeout = 5 * time.Second

// The controller owns the cap. Reading its live map keeps cap changes out of
// server configuration and its Pod checksum.
func (s *settingsRows) runCap(ctx context.Context, o *serverOwners) {
	if o.cluster.Kube == nil {
		s.observe(apicontract.SettingParallelRuns, "unavailable")
		return
	}
	probe, cancel := context.WithTimeout(ctx, settingsStatusTimeout)
	defer cancel()
	cm, err := o.cluster.Kube.CoreV1().ConfigMaps(o.c.config.ArgoNamespace).Get(probe, "oberth-controller-config", meta.GetOptions{})
	if err != nil {
		s.observe(apicontract.SettingParallelRuns, "unavailable")
		return
	}
	cap, err := strconv.Atoi(cm.Data["parallelism"])
	if err != nil || cap < 0 {
		s.observe(apicontract.SettingParallelRuns, "unavailable")
		return
	}
	s.add(apicontract.SettingParallelRuns, cap)
	origin := cm.Annotations[parallelOriginAnnotation]
	if origin != "default" && origin != "chosen" {
		origin = ""
	}
	s.values[len(s.values)-1].Source = origin
}
