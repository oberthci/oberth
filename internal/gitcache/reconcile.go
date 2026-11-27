package gitcache

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

const (
	reconciliationFile  = "oberth-reconcile"
	reconciliationRefs  = 2 * sourcecontract.MaxRefs
	reconciliationBytes = 4 << 20
)

// Only names whose public heads belonged to the old mirror are recorded. Local
// accepted heads are excluded before any ref moves; no object or custody is stored.
func markReconciliation(p string, updates []RefUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	names := make([]string, 0, len(updates))
	for _, u := range updates {
		names = append(names, u.Ref)
	}
	slices.Sort(names)
	return writeDurable(filepath.Join(p, reconciliationFile), []byte(strings.Join(names, "\n")+"\n"))
}

func clearReconciliation(p string) error {
	if err := os.Remove(filepath.Join(p, reconciliationFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	dir, err := os.Open(p)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (c *Cache) reconcileMirror(ctx context.Context, p string) error {
	f, err := os.Open(filepath.Join(p, reconciliationFile))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	noteReadWait(ctx)
	defer sourcedelivery.Measure(ctx, sourcedelivery.Recovery, true)()
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, reconciliationBytes+1))
	if err != nil {
		return err
	}
	names := strings.Fields(string(data))
	if len(data) > reconciliationBytes || len(names) > reconciliationRefs {
		return markerUnavailable(nil)
	}
	mirror, err := c.tracking(ctx, p)
	if err != nil {
		return err
	}
	return c.reconcileNames(ctx, p, names, mirror)
}
func (c *Cache) reconcileNames(ctx context.Context, p string, names []string, mirror map[string]string) error {
	public, err := c.refInventory(ctx, p)
	if err != nil {
		return err
	}
	var deletes, updates []RefUpdate
	for _, name := range names {
		if err = sourcecontract.ValidatePublicRef(name); err != nil {
			return markerUnavailable(err)
		}
		if oid := mirror[name]; oid != "" {
			updates = append(updates, RefUpdate{Ref: name, NewSHA: oid})
		} else if public[name] != "" {
			deletes = append(deletes, RefUpdate{Ref: name, OldSHA: public[name]})
		}
	}
	if err = c.refTransaction(ctx, p, deletes); err != nil {
		return err
	}
	if err = c.refTransaction(ctx, p, updates); err != nil {
		return err
	}

	return clearReconciliation(p)
}

func (c *Cache) recoverMirrors(ctx context.Context) error {
	paths, err := c.repositories()
	if err != nil {
		return err
	}
	recovery, cancel := context.WithTimeout(ctx, c.budgets.local)
	defer cancel()
	for _, p := range paths {
		// Each operation retries recovery under its own repository gate. A bad
		// mirror must not stop unrelated repositories from opening and serving.
		if c.lock(recovery, p) == nil {
			c.unlock(p)
		}
	}
	return ctx.Err()
}

// A corrupt internal marker is an operator recovery failure, not an author
// input refusal. Keep the underlying validation text as detail, not its class.
type reconciliationFailure struct{ detail string }

func (e *reconciliationFailure) Error() string {
	text := reasonRecovery + ". " + remedyRecovery
	if e.detail != "" {
		text += " " + e.detail
	}
	return text
}

func markerUnavailable(cause error) error {
	detail := ""
	var invalid *sourcecontract.InvalidError
	if errors.As(cause, &invalid) {
		detail = invalid.Reason()
	}
	return unavailable("recover repository mirror", &reconciliationFailure{detail: detail})
}
