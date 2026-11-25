package api

import (
	"context"
	"crypto/x509"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"io"
	"time"
)

// Authenticator distinguishes invalid credentials from temporary lookup failures.
// A successful result must contain a stable private credential ID and live actor.
type Authenticator interface {
	Bearer(context.Context, string) (apicontract.Caller, error)
	Certificate(context.Context, *x509.Certificate) (apicontract.Caller, error)
}

// Streams supplies independently authorized native output and backup archives.
// CheckLog repeats live target authorization; Close must unblock a pending Read.
type Streams interface {
	OpenLog(context.Context, apicontract.Caller, apicontract.ContainerArgs) (io.ReadCloser, error)
	CheckLog(context.Context, apicontract.Caller, string) error
	Backup(context.Context, apicontract.Caller, apicontract.Empty) (io.ReadCloser, error)
}

// AccountSnapshot authenticates a projected account token for this one stream.
// It cannot grant administrator or ordinary repository access.
type AccountSnapshot interface {
	Snapshot(ctx context.Context, remote, token string) (io.ReadCloser, error)
}

// Runtime injects time, tick delivery and nonces; Stop must release ticker work.
type Runtime struct {
	Now    func() time.Time
	Ticker func(time.Duration) (<-chan time.Time, func())
	Nonce  func() string
}

type Outcome string

const (
	Preparing    Outcome = "preparing"
	Complete     Outcome = "complete"
	Failed       Outcome = "failed"
	Final        Outcome = "final"
	Disconnected Outcome = "disconnected"
)

// Diagnostic contains only bounded transport facts. No caller, arguments, target,
// request ID, headers, payload or dependency error is accepted by this interface.
type Diagnostic struct {
	Reference string    `json:"reference"`
	Tool      string    `json:"tool"`
	Transport string    `json:"transport"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Outcome   Outcome   `json:"outcome"`
	Frames    uint16    `json:"frames"`
}

// Diagnostics receives a fixed-schema observation, never a raw error.
type Diagnostics interface{ Record(Diagnostic) }

type Classifier func(error) *publicerr.Error
