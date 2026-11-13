package main

import (
	"context"

	"github.com/oberthci/oberth/internal/service"
)

func runSchedulerAfterActivation(ctx context.Context, scheduler *service.Scheduler, activate func(context.Context) error, activated chan<- struct{}) error {
	if activate != nil {
		if err := activate(ctx); err != nil {
			return err
		}
	}
	close(activated)
	return scheduler.Run(ctx)
}
