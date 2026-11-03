package main

import (
	"context"
	"net/http"

	"github.com/oberthci/oberth/internal/model"
)

// newAdminGateServer serves the live daemon's private admin socket. Besides
// the audit mutation gate, it accepts one body-less wake: an in-pod run
// cancel committed its obligation directly to the database and asks the
// scheduler to run its cancellation pass now (withdraw the run's admission
// reservation, delete its Workflow) instead of on the next queue event. The
// wake carries no input and performs no mutation of its own.
func newAdminGateServer(gate func(context.Context) error, databasePath string, wake func(), counts ...func(context.Context) (model.IdleQueueStatus, error)) *http.Server {
	return newAdminGateServerWithTokenPolicy(gate, databasePath, wake, model.DefaultUplinkTokenPolicy(), counts...)
}
