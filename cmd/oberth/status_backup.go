package main

import (
	"context"
	"time"

	batch "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (s *settingsRows) backupJob(ctx context.Context, owner *serverOwners) {
	if owner.cluster.Kube == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
	defer cancel()
	job, err := owner.cluster.Kube.BatchV1().CronJobs(owner.c.config.Namespace).Get(ctx, "oberth-backup", meta.GetOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		s.observe("backup job", "unavailable")
		return
	}
	s.observe("backup job", backupJobState(job))
	last := "never"
	if job.Status.LastSuccessfulTime != nil {
		last = job.Status.LastSuccessfulTime.UTC().Format(time.RFC3339)
	}
	s.observe("backup last success", last)
}

func backupJobState(job *batch.CronJob) string {
	switch {
	case job.Spec.Suspend != nil && *job.Spec.Suspend:
		return "suspended"
	case len(job.Status.Active) > 0:
		return "running"
	case job.Status.LastScheduleTime != nil && (job.Status.LastSuccessfulTime == nil || job.Status.LastScheduleTime.After(job.Status.LastSuccessfulTime.Time)):
		return "missed run"
	case job.Status.LastSuccessfulTime != nil:
		return "succeeded"
	default:
		return "never run"
	}
}
