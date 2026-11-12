package app

import (
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// EffectivePriority freezes aging at the durable admission timestamp.
func workflowPriority(run model.Run) model.RunPriority {
	priority, _ := run.EffectivePriority(time.Now())
	return priority
}
