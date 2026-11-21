package stateport

import (
	"context"
	"io"
)

// Backup snapshots one configured database and inspects/restores its archives.
// Implementations verify schema, integrity, foreign keys and audit themselves;
// archive metadata never substitutes for inspecting the database.
type Backup interface {
	// Snapshot streams a bounded archive of a verified committed view including WAL;
	// shared-parent staging serialization and active-staging cleanup follow C7 invariant 9.
	// Success completes emission (the caller owns sink durability); an error leaves
	// discardable partial output, and retry takes a new view.
	Snapshot(context.Context, Options, io.Writer) (BackupReport, error)
	// Inspect verifies the complete archive and local database without migration
	// or writes. Either outcome leaves state unchanged; success grants no target
	// selection or external-continuity authority.
	Inspect(context.Context, Options, io.Reader) (BackupReport, error)
	// Restore verifies then publishes a complete sibling-staged file without
	// replacement; success includes file and parent-directory durability.
	// Before publication failure creates no destination; after publication an error,
	// cancellation or crash may leave a complete destination, so Conflict is not
	// successful replay and C9 must inspect it before applying the selected backup.
	Restore(context.Context, RestoreRequest, io.Reader) (BackupReport, error)
}

// RestoreRequest selects an absent offline database path. Its parent must
// already exist and be writable; sibling staging stays on that filesystem.
// C9 supplies the path and owns applying the completed staging database.
type RestoreRequest struct {
	Options     Options `json:"options"`
	Destination string  `json:"destination"`
}
