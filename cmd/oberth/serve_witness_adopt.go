package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

var witnessGenesisAcknowledgmentPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}:[0-9a-f]{64}$`)

const (
	witnessGenesisAdoptActor        = "operator"
	witnessGenesisAdoptAction       = "witness-genesis.adopted"
	witnessGenesisAdoptResourceType = "audit-witness-chain"
)

type witnessGenesisAdoptDetails struct {
	BaselineAuditID int64  `json:"baselineAuditID"`
	BaselineSHA256  string `json:"baselineSHA256"`
	Acknowledgment  string `json:"acknowledgment"`
}

// witnessGenesisAdoption carries the operator's one-shot --accept-witness-genesis
// acknowledgment. The zero value disables adoption entirely and preserves the
// fail-closed startup behavior byte for byte.
type witnessGenesisAdoption struct {
	baselineID     int64
	baselineSHA256 []byte
	chain          abandonedChainDiscoverer
	logger         *log.Logger
}

func (adoption witnessGenesisAdoption) requested() bool { return adoption.baselineID > 0 }

func (adoption witnessGenesisAdoption) printf(format string, args ...any) {
	if adoption.logger != nil {
		adoption.logger.Printf(format, args...)
	}
}

// adoptWitnessGenesis implements the witness-genesis adoption described in
// design-witness-adoption.md. Every refusal leaves the database byte-exact;
// writes begin only at step 8.
func adoptWitnessGenesis(
	ctx context.Context,
	path string,
	continuity startupContinuity,
	adoption witnessGenesisAdoption,
) (applied bool, database *store.Store, err error) {
	// Step 1: Inspect read-only.
	inspection, openErr := store.InspectCurrent(ctx, path, configuredStoreOptions(ctx))
	if openErr != nil {
		return false, nil, nil // schema error: normal startup path decides
	}

	// Step 2: Verify the complete local chain.
	head, headErr := inspection.VerifyAuditState(ctx)
	if headErr != nil {
		_ = inspection.Close()
		return false, nil, nil // verification error: normal path surfaces it
	}

	// Step 3: One-shot no-op check (stale flag).
	intents, intentsErr := continuity.Intents(ctx)
	if intentsErr != nil {
		_ = inspection.Close()
		return false, nil, fmt.Errorf("witness genesis adoption: read intents: %w", intentsErr)
	}
	pinned, pinnedErr := continuity.Pinned(ctx)
	if pinnedErr != nil {
		_ = inspection.Close()
		return false, nil, fmt.Errorf("witness genesis adoption: read continuity: %w", pinnedErr)
	}
	if len(intents) > 0 || len(pinned) > 0 {
		adoption.printf("accept-witness-genesis is set but rollback-external witness evidence already exists (intents=%d completions=%d); ignoring the one-shot flag",
			len(intents), len(pinned))
		_ = inspection.Close()
		return false, nil, nil
	}

	// Step 4: Resume detection (crash between acknowledgment append and intent creation).
	tail, tailErr := inspection.TailAuditAction(ctx)
	if tailErr == nil && tail.Action == witnessGenesisAdoptAction && tail.ResourceType == witnessGenesisAdoptResourceType {
		// Parse the details to verify the flag matches.
		var details witnessGenesisAdoptDetails
		if parseErr := json.Unmarshal([]byte(tail.Details), &details); parseErr != nil {
			_ = inspection.Close()
			return false, nil, fmt.Errorf("witness genesis adoption resume: parse acknowledgment details: %w", parseErr)
		}
		flagMatchesBaseline := adoption.baselineID == details.BaselineAuditID &&
			hex.EncodeToString(adoption.baselineSHA256) == details.BaselineSHA256
		flagMatchesAckHead := adoption.baselineID == tail.ID &&
			bytes.Equal(adoption.baselineSHA256, tail.SHA256)
		if !flagMatchesBaseline && !flagMatchesAckHead {
			_ = inspection.Close()
			return false, nil, fmt.Errorf(
				"refuse witness genesis resume: the local database already records adoption at baseline %d:%s "+
					"(acknowledgment head %d:%s) but --accept-witness-genesis=%d:%s was provided",
				details.BaselineAuditID, details.BaselineSHA256,
				tail.ID, hex.EncodeToString(tail.SHA256),
				adoption.baselineID, hex.EncodeToString(adoption.baselineSHA256))
		}

		// Step 5 on resume: TSA-checkpoint refusal (belt-and-braces).
		_, anchorErr := inspection.LatestAuditAnchor(ctx)
		if anchorErr == nil {
			_ = inspection.Close()
			return false, nil, errors.New("witness genesis adoption over existing timestamp-authority checkpoints is not supported yet; " +
				"the audit_anchors history predates witnessing and cannot satisfy witness verification (tracked: witness-adoption phase 2)")
		}
		if !errors.Is(anchorErr, store.ErrNotFound) {
			_ = inspection.Close()
			return false, nil, fmt.Errorf("witness genesis adoption resume: read audit anchors: %w", anchorErr)
		}

		// Step 7 on resume: Rekor identity check.
		_ = inspection.Close()
		if err := checkNoPublicHistory(ctx, adoption); err != nil {
			return false, nil, err
		}

		// Resume: open writable, skip append, prepare intent.
		database, err = store.OpenCurrent(ctx, path, configuredStoreOptions(ctx))
		if err != nil {
			return false, nil, err
		}
		if err := prepareWitnessGenesisIntent(ctx, continuity, tail.ID, tail.SHA256); err != nil {
			return false, nil, errors.Join(err, database.Close())
		}
		warnWitnessGenesisAdoption(adoption, details.BaselineAuditID, head.ID, tail.ID, true)
		return true, database, nil
	}

	// Step 5: TSA-checkpoint refusal (phase 1 scope).
	_, anchorErr := inspection.LatestAuditAnchor(ctx)
	if anchorErr == nil {
		_ = inspection.Close()
		return false, nil, errors.New("witness genesis adoption over existing timestamp-authority checkpoints is not supported yet; " +
			"the audit_anchors history predates witnessing and cannot satisfy witness verification (tracked: witness-adoption phase 2)")
	}
	if !errors.Is(anchorErr, store.ErrNotFound) {
		_ = inspection.Close()
		return false, nil, fmt.Errorf("witness genesis adoption: read audit anchors: %w", anchorErr)
	}

	// Step 6: Exact-head acknowledgment check.
	if head.ID != adoption.baselineID || !bytes.Equal(head.SHA256, adoption.baselineSHA256) {
		_ = inspection.Close()
		return false, nil, fmt.Errorf(
			"refuse witness genesis adoption: --accept-witness-genesis=%d:%s does not acknowledge "+
				"the current audit chain head %d:%s; read the current head (oberth audit head) and "+
				"acknowledge it exactly",
			adoption.baselineID, hex.EncodeToString(adoption.baselineSHA256),
			head.ID, hex.EncodeToString(head.SHA256))
	}

	// Step 7: Public-history refusal.
	_ = inspection.Close()
	if err := checkNoPublicHistory(ctx, adoption); err != nil {
		return false, nil, err
	}

	// Step 8: Open writable.
	database, err = store.OpenCurrent(ctx, path, configuredStoreOptions(ctx))
	if err != nil {
		return false, nil, err
	}

	// Step 9: Append the acknowledgment.
	detailsJSON, err := json.Marshal(witnessGenesisAdoptDetails{
		BaselineAuditID: adoption.baselineID,
		BaselineSHA256:  hex.EncodeToString(adoption.baselineSHA256),
		Acknowledgment:  "--accept-witness-genesis",
	})
	if err != nil {
		return false, nil, errors.Join(fmt.Errorf("encode witness genesis acknowledgment: %w", err), database.Close())
	}
	action, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor:        witnessGenesisAdoptActor,
		Action:       witnessGenesisAdoptAction,
		ResourceType: witnessGenesisAdoptResourceType,
		ResourceID:   hex.EncodeToString(adoption.baselineSHA256),
		Details:      string(detailsJSON),
	})
	if err != nil {
		return false, nil, errors.Join(fmt.Errorf("record witness genesis acknowledgment: %w", err), database.Close())
	}
	if action.ID != adoption.baselineID+1 {
		return false, nil, errors.Join(fmt.Errorf("witness genesis acknowledgment became audit action %d, want %d "+
			"(the chain advanced concurrently)", action.ID, adoption.baselineID+1), database.Close())
	}

	// Step 10: Prepare the sequence-1 intent.
	if err := prepareWitnessGenesisIntent(ctx, continuity, action.ID, action.SHA256); err != nil {
		return false, nil, errors.Join(err, database.Close())
	}

	// Step 11: Warn loudly.
	warnWitnessGenesisAdoption(adoption, adoption.baselineID, head.ID, action.ID, false)

	return true, database, nil
}

func checkNoPublicHistory(ctx context.Context, adoption witnessGenesisAdoption) error {
	tip, found, err := adoption.chain.AbandonedChainTip(ctx)
	if err != nil {
		return fmt.Errorf("witness genesis adoption: discover public witness history: %w", err)
	}
	if found {
		return fmt.Errorf(
			"refuse witness genesis adoption: this deployment's witness identity already has "+
				"published Rekor history (latest %s, log index %d, integrated %s, audit head %d); "+
				"adoption would bridge over existing public witness evidence — if local witness "+
				"state was lost, investigate before proceeding; adoption is only for deployments "+
				"that have never witnessed",
			tip.UUID, tip.LogIndex, tip.IntegratedAt.UTC().Format("2006-01-02T15:04:05Z"), tip.AuditID)
	}
	return nil
}

func prepareWitnessGenesisIntent(ctx context.Context, continuity startupContinuity, actionID int64, actionSHA256 []byte) error {
	return continuity.Prepare(ctx, model.AuditWitnessIntent{
		Sequence:    1,
		AuditID:     actionID,
		AuditSHA256: append([]byte(nil), actionSHA256...),
	})
}

func warnWitnessGenesisAdoption(adoption witnessGenesisAdoption, baselineID, priorActionCount, acknowledgmentID int64, resumed bool) {
	mode := "adopting"
	if resumed {
		mode = "resuming adoption of"
	}
	adoption.printf(
		"WARNING: %s witness genesis: external witnessing starts at audit action %d/%s; all %d prior audit action(s) are trusted by this explicit operator acknowledgment and are hash-committed by the first published witness",
		mode, acknowledgmentID, hex.EncodeToString(adoption.baselineSHA256), baselineID)
	adoption.printf("WARNING: the acknowledgment is recorded permanently as audit action %d", acknowledgmentID)
	adoption.printf("WARNING: witness genesis adoption is a one-shot acknowledgment: remove --accept-witness-genesis (helm value auditAnchor.acceptWitnessGenesis) after startup succeeds")
	adoption.printf("WARNING: ensure no other Oberth instance uses the same SSH host key; a concurrent instance would publish under the same witness identity and fork the chain")
}

// parseWitnessGenesisFlag parses a validated "<auditID>:<sha256hex>" string
// into its integer and binary components.
func parseWitnessGenesisFlag(value string) (int64, []byte, error) {
	parts := splitOnce(value, ':')
	if len(parts) != 2 {
		return 0, nil, fmt.Errorf("invalid witness genesis format: %q", value)
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, nil, fmt.Errorf("invalid witness genesis audit ID: %q", parts[0])
	}
	sha, err := hex.DecodeString(parts[1])
	if err != nil || len(sha) != 32 {
		return 0, nil, fmt.Errorf("invalid witness genesis SHA-256: %q", parts[1])
	}
	return id, sha, nil
}

func splitOnce(s string, sep byte) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}
