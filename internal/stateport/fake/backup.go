// Package fake provides in-memory durable run facts and backup over a verified seed database.
// Spec: docs/plans/c7-durable-state.md#baseline-and-backup
package fake

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// Backup recognizes its supplied seed bytes as a verified database. It models
// archive/restore semantics; SQL verification belongs to the real implementation.
type Backup struct {
	mu       sync.Mutex
	protocol stateport.Protocol
	report   stateport.BackupReport
	database []byte
	restored map[string][]byte
}

// New copies one trusted verified fixture; different fixture bytes are refused.
func New(protocol stateport.Protocol, report stateport.BackupReport, database []byte) (*Backup, error) {
	if err := protocol.Validate(); err != nil {
		return nil, err
	}
	if err := report.Validate(); err != nil {
		return nil, err
	}
	if len(database) == 0 {
		return nil, publicerr.New(publicerr.InvalidInput, "seed database is empty", "Supply verified fixture bytes.", nil)
	}
	return &Backup{protocol: protocol, report: report, database: bytes.Clone(database), restored: make(map[string][]byte)}, nil
}

func (b *Backup) check(ctx context.Context, options stateport.Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := options.Validate(); err != nil {
		return err
	}
	if b.protocol != options.Protocol || b.report.Schema != options.Schema {
		return publicerr.New(publicerr.SchemaRefused, "configured database protocol or schema differs", "Select the original database protocol and supported schema.", nil)
	}
	limit, err := options.Limit()
	if err != nil {
		return err
	}
	if int64(len(b.database)) > limit {
		return publicerr.New(publicerr.InvalidInput, fmt.Sprintf("database is %d bytes; limit is %d bytes", len(b.database), limit), "Raise the database limit within 4 GiB or use a smaller backup.", nil)
	}
	return nil
}

func (b *Backup) Snapshot(ctx context.Context, options stateport.Options, output io.Writer) (stateport.BackupReport, error) {
	if err := b.check(ctx, options); err != nil {
		return stateport.BackupReport{}, err
	}
	if output == nil {
		return stateport.BackupReport{}, publicerr.New(publicerr.InvalidInput, "backup output is missing", "Supply a writable backup destination.", nil)
	}
	archive := b.payload()
	_, err := io.Copy(contextWriter{ctx, output}, bytes.NewReader(archive))
	if err != nil {
		return stateport.BackupReport{}, publicerr.New(publicerr.Unavailable, "backup output could not be completed", "Discard partial output and retry the backup.", err)
	}
	if err := ctx.Err(); err != nil {
		return stateport.BackupReport{}, err
	}
	return b.report, nil
}

func (b *Backup) Inspect(ctx context.Context, options stateport.Options, input io.Reader) (stateport.BackupReport, error) {
	if err := b.check(ctx, options); err != nil {
		return stateport.BackupReport{}, err
	}
	if input == nil {
		return stateport.BackupReport{}, publicerr.New(publicerr.InvalidInput, "backup input is missing", "Supply a complete backup.", nil)
	}
	expected := b.payload()
	body, err := io.ReadAll(io.LimitReader(contextReader{ctx, input}, int64(len(expected))+1))
	if err != nil {
		return stateport.BackupReport{}, publicerr.New(publicerr.Unavailable, "backup input could not be read", "Retry with a complete readable backup.", err)
	}
	if err := ctx.Err(); err != nil {
		return stateport.BackupReport{}, err
	}
	if !bytes.Equal(body, expected) {
		return stateport.BackupReport{}, publicerr.New(publicerr.SchemaRefused, "backup is not the configured verified fixture", "Supply the complete backup for this protocol and schema.", nil)
	}
	return b.report, nil
}

func (b *Backup) Restore(ctx context.Context, request stateport.RestoreRequest, input io.Reader) (stateport.BackupReport, error) {
	if request.Destination == "" {
		return stateport.BackupReport{}, publicerr.New(publicerr.InvalidInput, "restore destination is missing", "Choose a new offline staging destination.", nil)
	}
	report, err := b.Inspect(ctx, request.Options, input)
	if err != nil {
		return stateport.BackupReport{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return stateport.BackupReport{}, err
	}
	if _, exists := b.restored[request.Destination]; exists {
		return stateport.BackupReport{}, publicerr.New(publicerr.Conflict, "restore destination already exists", "Choose a new staging destination; inspect existing output before removing it.", nil)
	}
	b.restored[request.Destination] = bytes.Clone(b.database)
	return report, nil
}
