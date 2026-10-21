package service

import (
	"context"
	"log"

	"github.com/oberthci/oberth/internal/model"
)

type RunTimingReader interface {
	RunTiming(context.Context, string) (model.RunTiming, error)
}

func (scheduler *Scheduler) timingMilestone(ctx context.Context, runID, phase string) {
	if recorder, ok := scheduler.store.(interface {
		RecordRunTimingMilestone(context.Context, string, string) error
	}); ok {
		if recorder.RecordRunTimingMilestone(ctx, runID, phase) != nil {
			log.Print("WARN: run phase timing observation unavailable")
		}
	}
}
func (scheduler *Scheduler) admissionRequested(ctx context.Context, runID string) {
	if recorder, ok := scheduler.store.(interface {
		RecordAdmissionRequested(context.Context, string) error
	}); ok {
		if recorder.RecordAdmissionRequested(ctx, runID) != nil {
			log.Print("WARN: admission request timing unavailable")
		}
	}
}

func (history scopedHistory) RunTiming(ctx context.Context, id string) (model.RunTiming, error) {
	run, err := history.Run(ctx, id)
	if err != nil {
		return model.RunTiming{}, err
	}
	if reader, ok := history.inner.(RunTimingReader); ok {
		return reader.RunTiming(ctx, id)
	}
	return model.RunTiming{Version: model.TimingVersion, RunID: id, QueuedAt: run.QueuedAt, Unavailable: "not-recorded", Admissions: []model.AdmissionTiming{}, Steps: []model.StepTiming{}}, nil
}
