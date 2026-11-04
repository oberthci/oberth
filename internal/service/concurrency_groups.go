package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

func (scheduler *Scheduler) concurrencyDefinitions(ctx context.Context) ([]model.AdmissionGroup, error) {
	if reader, ok := scheduler.store.(interface {
		ConcurrencyGroups(context.Context) ([]model.AdmissionGroup, error)
	}); ok {
		return reader.ConcurrencyGroups(ctx)
	}
	return nil, nil
}

// ConcurrencyGroups is a read-only snapshot. SQL supplies YAML definitions;
// only the gate can say which runs currently own permits.
func (scheduler *Scheduler) ConcurrencyGroups(ctx context.Context) ([]model.AdmissionGroup, error) {
	definitions, err := scheduler.concurrencyDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	return scheduler.admissionState(definitions).Groups, nil
}

func concurrencyGroupMessage(admission *model.AdmissionObservation, abbreviated bool) string {
	displayID := func(id string) string {
		if abbreviated {
			return shortRunID(id)
		}
		return id
	}
	holders := make([]string, 0, len(admission.GroupHolders))
	for _, id := range admission.GroupHolders {
		holders = append(holders, displayID(id))
	}
	if len(holders) == 0 && admission.GroupHolder != "" {
		holders = append(holders, displayID(admission.GroupHolder))
	}
	current := strings.Join(holders, ", ")
	if current == "" {
		current = "none visible"
	}
	reason := fmt.Sprintf("concurrency group %q busy (limit %d; holders: %s)", admission.Group, admission.GroupLimit, current)
	if admission.GroupAhead != "" {
		reason += ", behind run " + displayID(admission.GroupAhead)
	}
	return reason
}
