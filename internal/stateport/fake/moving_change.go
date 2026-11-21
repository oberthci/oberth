package fake

import (
	"context"
	"github.com/oberthci/oberth/internal/stateport"
)

func (f *Runs) movingChange(ctx context.Context, name string, events []stateport.EventWrite, apply func(*runMemory) error) error {
	if err := f.before(ctx, name, true); err != nil {
		return err
	}
	return f.transact(ctx, events, apply)
}
