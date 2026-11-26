package main

import (
	"testing"
	"time"

	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kfake "k8s.io/client-go/kubernetes/fake"
)

func TestStatusShowsMissedBackupAndLastNativeSuccess(t *testing.T) {
	last := meta.NewTime(time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC))
	next := meta.NewTime(last.Add(24 * time.Hour))
	for _, row := range []struct {
		name   string
		status batch.CronJobStatus
		want   string
		last   string
	}{
		{"missed", batch.CronJobStatus{LastScheduleTime: &next, LastSuccessfulTime: &last}, "missed run", "2026-11-25T02:00:00Z"},
		{"never-succeeded", batch.CronJobStatus{LastScheduleTime: &next}, "missed run", "never"},
		{"running", batch.CronJobStatus{LastScheduleTime: &next, LastSuccessfulTime: &last, Active: []core.ObjectReference{{Name: "backup"}}}, "running", "2026-11-25T02:00:00Z"},
		{"succeeded", batch.CronJobStatus{LastScheduleTime: &last, LastSuccessfulTime: &last}, "succeeded", "2026-11-25T02:00:00Z"},
	} {
		t.Run(row.name, func(t *testing.T) {
			job := &batch.CronJob{ObjectMeta: meta.ObjectMeta{Name: "oberth-backup", Namespace: "oberth"}, Status: row.status}
			owner := &serverOwners{c: &composition{config: serverConfig{Namespace: "oberth"}}, cluster: clusterClients{Kube: kfake.NewClientset(job)}}
			var rows settingsRows
			rows.backupJob(t.Context(), owner)
			if len(rows.values) != 2 || rows.values[0].Value != row.want || rows.values[1].Value != row.last || rows.values[0].Source != "" {
				t.Fatal("native backup outcome was hidden or invented", rows.values)
			}
		})
	}
}
