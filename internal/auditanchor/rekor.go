package auditanchor

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	openapiruntime "github.com/go-openapi/runtime"
	"github.com/go-openapi/strfmt"
	rekorclient "github.com/sigstore/rekor/pkg/client"
	generatedclient "github.com/sigstore/rekor/pkg/generated/client"
	"github.com/sigstore/rekor/pkg/generated/client/entries"
	"github.com/sigstore/rekor/pkg/generated/client/index"
	"github.com/sigstore/rekor/pkg/generated/models"
	"github.com/sigstore/rekor/pkg/sharding"
	"github.com/sigstore/rekor/pkg/types"
	"github.com/sigstore/rekor/pkg/types/hashedrekord"
	hashedrekordv001 "github.com/sigstore/rekor/pkg/types/hashedrekord/v0.0.1"
	rekorverify "github.com/sigstore/rekor/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/tuf"

	"github.com/oberthci/oberth/internal/model"
)

const (
	rekorRequestTimeout          = 15 * time.Second
	witnessKeyInfo               = "cloudtaser-oberth-audit-witness-v1"
	maximumRekorBody             = 1 << 20
	witnessMetadataHost          = "audit-witness.oberth.invalid"
	maximumIdentitySearchResults = 512
)

type rekorAPI interface {
	SearchExact(context.Context, []byte, []byte) ([]string, error)
	SearchIdentity(context.Context, string) ([]string, error)
	Get(context.Context, string) (string, models.LogEntryAnon, error)
	Create(context.Context, models.ProposedEntry) (string, models.LogEntryAnon, error)
}

type rekorEntryVerifier interface {
	Verify(context.Context, *models.LogEntryAnon) error
}

type RekorWitness struct {
	api       rekorAPI
	verifier  rekorEntryVerifier
	private   *ecdsa.PrivateKey
	publicDER []byte
	identity  string

	mu            sync.Mutex
	verified      map[string]model.AuditWitness
	historyLoaded bool
	tip           string
}

type witnessCertificateSigner struct{ private *ecdsa.PrivateKey }

func (signer witnessCertificateSigner) Public() crypto.PublicKey { return &signer.private.PublicKey }

func (signer witnessCertificateSigner) Sign(_ io.Reader, digest []byte, options crypto.SignerOpts) ([]byte, error) {
	return signer.private.Sign(nil, digest, options)
}

func NewRekorWitness(ctx context.Context, endpoint string, hostKeyMaterial []byte) (*RekorWitness, error) {
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("audit anchor: Rekor endpoint must be an absolute HTTPS URL without credentials or a fragment")
	}
	client, err := rekorclient.GetRekorClient(parsed.String(),
		rekorclient.WithUserAgent("oberth-audit-witness"), rekorclient.WithRetryCount(2))
	if err != nil {
		return nil, fmt.Errorf("audit anchor: create Rekor client: %w", err)
	}
	verifier, err := loadTUFRekorVerifier(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit anchor: load trusted Rekor keys: %w", err)
	}
	return newRekorWitness(&generatedRekorAPI{client: client}, verifier, hostKeyMaterial)
}

func newRekorWitness(api rekorAPI, verifier rekorEntryVerifier, keyMaterial []byte) (*RekorWitness, error) {
	if api == nil || verifier == nil {
		return nil, errors.New("audit anchor: Rekor API and verifier are required")
	}
	privateKey, err := deriveWitnessKey(keyMaterial)
	if err != nil {
		return nil, err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("audit anchor: encode witness public key: %w", err)
	}
	identityHash := sha256.Sum256(publicDER)
	identity := "audit-" + hex.EncodeToString(identityHash[:20]) + "@witness.oberth.invalid"
	return &RekorWitness{
		api: api, verifier: verifier, private: privateKey,
		publicDER: publicDER, identity: identity,
		verified: make(map[string]model.AuditWitness),
	}, nil
}

func deriveWitnessKey(keyMaterial []byte) (*ecdsa.PrivateKey, error) {
	if len(keyMaterial) == 0 {
		return nil, errors.New("audit anchor: persistent SSH host key material is required for the witness identity")
	}
	seed, err := hkdf.Key(sha256.New, keyMaterial, nil, witnessKeyInfo, 48)
	if err != nil {
		return nil, fmt.Errorf("audit anchor: derive witness key: %w", err)
	}
	curve := elliptic.P256()
	orderMinusOne := new(big.Int).Sub(curve.Params().N, big.NewInt(1))
	scalar := new(big.Int).SetBytes(seed)
	scalar.Mod(scalar, orderMinusOne)
	scalar.Add(scalar, big.NewInt(1))
	encoded := make([]byte, (curve.Params().N.BitLen()+7)/8)
	scalar.FillBytes(encoded)
	privateKey, err := ecdsa.ParseRawPrivateKey(curve, encoded)
	if err != nil {
		return nil, fmt.Errorf("audit anchor: parse derived witness key: %w", err)
	}
	return privateKey, nil
}

func (witness *RekorWitness) History(
	ctx context.Context,
	pinned []model.AuditWitness,
	head model.AuditHead,
) ([]model.AuditWitness, error) {
	witness.mu.Lock()
	defer witness.mu.Unlock()
	return witness.historyLocked(ctx, pinned, head)
}

var errForeignWitness = errors.New("audit anchor: unrelated Rekor identity result")

// NormalizeRekorUUID reduces a possibly shard-qualified Rekor entry UUID to its
// canonical base form and rejects malformed identifiers.
func NormalizeRekorUUID(uuid string) (string, error) {
	return normalizedRekorUUID(uuid)
}

// AbandonedChainTip enumerates every Rekor entry published under this witness
// identity, skips unrelated third-party entries that merely share the indexed
// identity, and returns the fully verified entry with the highest log index.
// It reports found=false when the identity has no public witness history. The
// method reads only public state and never mutates the recovery cache, so it is
// safe to call before any History recovery.
func (witness *RekorWitness) AbandonedChainTip(ctx context.Context) (model.AuditWitness, bool, error) {
	uuids, err := witness.api.SearchIdentity(ctx, witness.identity)
	if err != nil {
		return model.AuditWitness{}, false, fmt.Errorf("%w: search Rekor witness identity: %w", ErrWitnessUnavailable, err)
	}
	candidates := make([]string, 0, len(uuids))
	seen := make(map[string]struct{}, len(uuids))
	for _, uuid := range uuids {
		base, normalizeErr := normalizedRekorUUID(uuid)
		if normalizeErr != nil {
			return model.AuditWitness{}, false, normalizeErr
		}
		if _, duplicate := seen[base]; duplicate {
			continue
		}
		seen[base] = struct{}{}
		candidates = append(candidates, uuid)
	}
	if len(candidates) > maximumIdentitySearchResults {
		return model.AuditWitness{}, false, fmt.Errorf(
			"audit anchor: Rekor identity search returned %d entries, more than the supported %d",
			len(candidates), maximumIdentitySearchResults,
		)
	}
	var tip model.AuditWitness
	found := false
	for _, uuid := range candidates {
		actualUUID, entry, getErr := witness.api.Get(ctx, uuid)
		if getErr != nil {
			return model.AuditWitness{}, false, fmt.Errorf("%w: retrieve indexed Rekor witness %s: %w", ErrWitnessUnavailable, uuid, getErr)
		}
		requestedBase, normalizeErr := normalizedRekorUUID(uuid)
		if normalizeErr != nil {
			return model.AuditWitness{}, false, normalizeErr
		}
		actualBase, normalizeErr := normalizedRekorUUID(actualUUID)
		if normalizeErr != nil {
			return model.AuditWitness{}, false, normalizeErr
		}
		if actualBase != requestedBase {
			return model.AuditWitness{}, false, fmt.Errorf("audit anchor: Rekor returned witness %s for indexed record %s", actualUUID, uuid)
		}
		verified, verifyErr := witness.verifyEntry(ctx, actualUUID, &entry)
		if errors.Is(verifyErr, errForeignWitness) {
			continue
		}
		if verifyErr != nil {
			return model.AuditWitness{}, false, verifyErr
		}
		if !found || verified.LogIndex > tip.LogIndex {
			tip = verified
			found = true
		}
	}
	return tip, found, nil
}

func (witness *RekorWitness) historyLocked(
	ctx context.Context,
	pinned []model.AuditWitness,
	head model.AuditHead,
) ([]model.AuditWitness, error) {
	if head.ID < 0 || len(head.SHA256) != sha256.Size {
		return nil, errors.New("audit anchor: current chain head is invalid for witness recovery")
	}
	if err := validateContinuityHistory(pinned); err != nil {
		return nil, fmt.Errorf("audit anchor: validate rollback-external witness pins: %w", err)
	}
	discovered := make(map[string]model.AuditWitness, len(pinned)+1)
	for _, expected := range pinned {
		base, err := normalizedRekorUUID(expected.UUID)
		if err != nil {
			return nil, err
		}
		actualUUID, entry, getErr := witness.api.Get(ctx, expected.UUID)
		if getErr != nil {
			return nil, fmt.Errorf("%w: retrieve pinned Rekor witness %s: %w", ErrWitnessUnavailable, expected.UUID, getErr)
		}
		actualBase, normalizeErr := normalizedRekorUUID(actualUUID)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		if actualBase != base {
			return nil, fmt.Errorf("audit anchor: Rekor returned witness %s for pinned record %s", actualUUID, expected.UUID)
		}
		verified, err := witness.verifyEntry(ctx, actualUUID, &entry)
		if err != nil {
			return nil, err
		}
		if !sameAuditWitness(verified, expected) {
			return nil, fmt.Errorf("audit anchor: pinned Rekor witness %s differs from its verified public entry", expected.UUID)
		}
		discovered[base] = verified
	}
	history, tip, err := orderWitnessHistory(discovered)
	if err != nil {
		return nil, err
	}
	if len(history) == 0 || head.ID > history[len(history)-1].AuditID {
		certificatePEM, certificateErr := witness.certificatePEM(head, tip)
		if certificateErr != nil {
			return nil, certificateErr
		}
		uuids, searchErr := witness.api.SearchExact(ctx, certificatePEM, head.SHA256)
		if searchErr != nil {
			return nil, fmt.Errorf("%w: search exact Rekor witness: %w", ErrWitnessUnavailable, searchErr)
		}
		// The deterministic empty-chain genesis head is identical for every
		// fresh Oberth deployment, so the shared transparency log's search
		// index returns other identities' genesis witnesses alongside our own.
		// An entry that provably belongs to another witness key is not evidence
		// about this chain and is skipped; only an entry signed by this witness
		// key may complete the recovery. A publication of ours hiding beyond
		// the bounded scan is still recovered by the content-addressed
		// duplicate response of the next publish attempt.
		if len(uuids) > maximumIdentitySearchResults {
			uuids = uuids[:maximumIdentitySearchResults]
		}
		recovered := false
		for _, candidate := range uuids {
			actualUUID, entry, getErr := witness.api.Get(ctx, candidate)
			if getErr != nil {
				return nil, fmt.Errorf("%w: retrieve exact Rekor witness %s: %w", ErrWitnessUnavailable, candidate, getErr)
			}
			verified, verifyErr := witness.verifyEntry(ctx, actualUUID, &entry)
			if errors.Is(verifyErr, errForeignWitness) {
				continue
			}
			if verifyErr != nil {
				return nil, verifyErr
			}
			if recovered {
				return nil, errors.New("audit anchor: exact Rekor witness search returned multiple entries for this witness key")
			}
			if verified.AuditID != head.ID || !bytes.Equal(verified.AuditSHA256, head.SHA256) || verified.PreviousUUID != tip {
				return nil, errors.New("audit anchor: exact Rekor recovery returned another audit head or predecessor")
			}
			base, normalizeErr := normalizedRekorUUID(verified.UUID)
			if normalizeErr != nil {
				return nil, normalizeErr
			}
			discovered[base] = verified
			recovered = true
		}
		if recovered {
			history, tip, err = orderWitnessHistory(discovered)
			if err != nil {
				return nil, err
			}
		}
	}
	for base, cached := range witness.verified {
		if _, exists := discovered[base]; !exists {
			return nil, fmt.Errorf("audit anchor: recovery omitted previously verified witness %s", cached.UUID)
		}
	}
	for base, entry := range discovered {
		witness.verified[base] = entry
	}
	witness.historyLoaded = true
	witness.tip = tip
	return history, nil
}

func (witness *RekorWitness) Publish(ctx context.Context, head model.AuditHead) (model.AuditWitness, error) {
	witness.mu.Lock()
	defer witness.mu.Unlock()

	if head.ID < 0 || len(head.SHA256) != sha256.Size {
		return model.AuditWitness{}, errors.New("audit anchor: chain head is invalid for external witnessing")
	}
	if !witness.historyLoaded {
		return model.AuditWitness{}, errors.New("audit anchor: recover rollback-external witness pins before publication")
	}
	if current, exists := witness.verified[witness.tip]; exists && current.AuditID == head.ID && bytes.Equal(current.AuditSHA256, head.SHA256) {
		return current, nil
	}
	certificatePEM, err := witness.certificatePEM(head, witness.tip)
	if err != nil {
		return model.AuditWitness{}, err
	}
	signatureBytes, err := witness.private.Sign(nil, head.SHA256, crypto.SHA256)
	if err != nil {
		return model.AuditWitness{}, fmt.Errorf("audit anchor: sign chain head for Rekor: %w", err)
	}
	proposed, err := types.NewProposedEntry(ctx, hashedrekord.KIND, hashedrekordv001.APIVERSION, types.ArtifactProperties{
		ArtifactHash:   "sha256:" + hex.EncodeToString(head.SHA256),
		SignatureBytes: signatureBytes,
		PublicKeyBytes: [][]byte{certificatePEM},
		PKIFormat:      "x509",
	})
	if err != nil {
		return model.AuditWitness{}, fmt.Errorf("audit anchor: create Rekor witness entry: %w", err)
	}
	uuid, entry, err := witness.api.Create(ctx, proposed)
	if err != nil {
		return model.AuditWitness{}, fmt.Errorf("%w: publish Rekor witness: %w", ErrWitnessUnavailable, err)
	}
	verified, err := witness.verifyEntry(ctx, uuid, &entry)
	if err != nil {
		return model.AuditWitness{}, err
	}
	if verified.AuditID != head.ID || !bytes.Equal(verified.AuditSHA256, head.SHA256) || verified.PreviousUUID != witness.tip {
		return model.AuditWitness{}, errors.New("audit anchor: Rekor response commits a different audit head or predecessor")
	}
	base, err := normalizedRekorUUID(verified.UUID)
	if err != nil {
		return model.AuditWitness{}, err
	}
	witness.verified[base] = verified
	witness.tip = base
	return verified, nil
}

func (witness *RekorWitness) certificatePEM(head model.AuditHead, previousUUID string) ([]byte, error) {
	previous := previousUUID
	if previous == "" {
		previous = "root"
	}
	metadataURL := &url.URL{
		Scheme: "https", Host: witnessMetadataHost,
		Path: "/v1/" + strconv.FormatInt(head.ID, 10) + "/" + hex.EncodeToString(head.SHA256) + "/" + previous,
	}
	serialHash := sha256.Sum256([]byte(metadataURL.String()))
	serial := new(big.Int).SetBytes(serialHash[:20])
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: witness.identity},
		NotBefore:             time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		EmailAddresses:        []string{witness.identity},
		URIs:                  []*url.URL{metadataURL},
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(
		cryptorand.Reader, template, template, &witness.private.PublicKey,
		witnessCertificateSigner{private: witness.private},
	)
	if err != nil {
		return nil, fmt.Errorf("audit anchor: create linked witness certificate: %w", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	if len(encoded) == 0 {
		return nil, errors.New("audit anchor: encode linked witness certificate")
	}
	return encoded, nil
}

func orderWitnessHistory(discovered map[string]model.AuditWitness) ([]model.AuditWitness, string, error) {
	if len(discovered) == 0 {
		return nil, "", nil
	}
	children := make(map[string]string, len(discovered))
	root := ""
	for base, entry := range discovered {
		if entry.PreviousUUID == "" {
			if root != "" {
				return nil, "", errors.New("audit anchor: Rekor witness history has multiple roots")
			}
			root = base
			continue
		}
		if _, exists := discovered[entry.PreviousUUID]; !exists {
			return nil, "", fmt.Errorf("audit anchor: Rekor witness %s has missing predecessor %s", entry.UUID, entry.PreviousUUID)
		}
		if existing, exists := children[entry.PreviousUUID]; exists && existing != base {
			return nil, "", fmt.Errorf("audit anchor: Rekor witness history forks after %s", entry.PreviousUUID)
		}
		children[entry.PreviousUUID] = base
	}
	if root == "" {
		return nil, "", errors.New("audit anchor: Rekor witness history has no root")
	}
	history := make([]model.AuditWitness, 0, len(discovered))
	current := root
	for current != "" {
		entry := discovered[current]
		if len(history) > 0 && entry.AuditID <= history[len(history)-1].AuditID {
			return nil, "", fmt.Errorf("audit anchor: Rekor witness %s does not advance the audit sequence", entry.UUID)
		}
		history = append(history, entry)
		current = children[current]
	}
	if len(history) != len(discovered) {
		return nil, "", errors.New("audit anchor: Rekor witness history is cyclic or disconnected")
	}
	tip, err := normalizedRekorUUID(history[len(history)-1].UUID)
	if err != nil {
		return nil, "", err
	}
	return history, tip, nil
}

func sameAuditWitness(left, right model.AuditWitness) bool {
	return left.UUID == right.UUID && left.LogIndex == right.LogIndex && left.IntegratedAt.Equal(right.IntegratedAt) &&
		left.AuditID == right.AuditID && bytes.Equal(left.AuditSHA256, right.AuditSHA256) && left.PreviousUUID == right.PreviousUUID
}

func normalizedRekorUUID(uuid string) (string, error) {
	base, err := sharding.GetUUIDFromIDString(uuid)
	if err != nil || strings.TrimSpace(base) == "" {
		return "", fmt.Errorf("audit anchor: Rekor returned invalid witness UUID %q", uuid)
	}
	return base, nil
}

func (witness *RekorWitness) verifyEntry(ctx context.Context, uuid string, entry *models.LogEntryAnon) (model.AuditWitness, error) {
	baseUUID, err := normalizedRekorUUID(uuid)
	if err != nil {
		return model.AuditWitness{}, err
	}
	if entry == nil || entry.LogIndex == nil || *entry.LogIndex < 0 || entry.IntegratedTime == nil || *entry.IntegratedTime <= 0 ||
		entry.LogID == nil || entry.Verification == nil || entry.Verification.InclusionProof == nil ||
		entry.Verification.InclusionProof.Checkpoint == nil || len(entry.Verification.SignedEntryTimestamp) == 0 {
		return model.AuditWitness{}, fmt.Errorf("audit anchor: Rekor witness %s lacks complete transparency proof", uuid)
	}
	if err := witness.verifier.Verify(ctx, entry); err != nil {
		return model.AuditWitness{}, fmt.Errorf("audit anchor: verify Rekor transparency proof for %s: %w", uuid, err)
	}
	bodyText, ok := entry.Body.(string)
	if !ok || len(bodyText) == 0 || len(bodyText) > base64.StdEncoding.EncodedLen(maximumRekorBody) {
		return model.AuditWitness{}, fmt.Errorf("%w: Rekor witness %s has an invalid body", errForeignWitness, uuid)
	}
	body, err := base64.StdEncoding.DecodeString(bodyText)
	if err != nil || len(body) > maximumRekorBody {
		return model.AuditWitness{}, fmt.Errorf("%w: decode Rekor witness %s body: %w", errForeignWitness, uuid, err)
	}
	proposed, err := models.UnmarshalProposedEntry(bytes.NewReader(body), openapiruntime.JSONConsumer())
	if err != nil {
		return model.AuditWitness{}, fmt.Errorf("%w: parse Rekor witness %s: %w", errForeignWitness, uuid, err)
	}
	implementation, err := types.UnmarshalEntry(proposed)
	if err != nil {
		return model.AuditWitness{}, fmt.Errorf("%w: validate Rekor witness %s signature: %w", errForeignWitness, uuid, err)
	}
	record, ok := implementation.(*hashedrekordv001.V001Entry)
	if !ok || record.HashedRekordObj.Data == nil || record.HashedRekordObj.Data.Hash == nil ||
		record.HashedRekordObj.Data.Hash.Algorithm == nil || record.HashedRekordObj.Data.Hash.Value == nil ||
		record.HashedRekordObj.Signature == nil || record.HashedRekordObj.Signature.Content == nil ||
		record.HashedRekordObj.Signature.PublicKey == nil || record.HashedRekordObj.Signature.PublicKey.Content == nil {
		return model.AuditWitness{}, fmt.Errorf("%w: Rekor witness %s is not a complete hashedrekord v0.0.1 entry", errForeignWitness, uuid)
	}
	if *record.HashedRekordObj.Data.Hash.Algorithm != models.HashedrekordV001SchemaDataHashAlgorithmSha256 {
		return model.AuditWitness{}, fmt.Errorf("%w: Rekor witness %s does not use SHA-256", errForeignWitness, uuid)
	}
	auditHash, err := hex.DecodeString(*record.HashedRekordObj.Data.Hash.Value)
	if err != nil || len(auditHash) != sha256.Size {
		// Reached before key ownership is proven: an entry whose hash field
		// cannot parse is by definition not one of this witness's entries.
		return model.AuditWitness{}, fmt.Errorf("%w: Rekor witness %s has an invalid audit hash", errForeignWitness, uuid)
	}
	auditID, previousUUID, err := witness.verifyCertificateMetadata(
		uuid, record.HashedRekordObj.Signature.PublicKey.Content, auditHash,
	)
	if err != nil {
		return model.AuditWitness{}, err
	}
	if !ecdsa.VerifyASN1(&witness.private.PublicKey, auditHash, record.HashedRekordObj.Signature.Content) {
		return model.AuditWitness{}, fmt.Errorf("audit anchor: Rekor witness %s has an invalid audit-head signature", uuid)
	}
	return model.AuditWitness{
		UUID: baseUUID, LogIndex: *entry.LogIndex, IntegratedAt: time.Unix(*entry.IntegratedTime, 0).UTC(),
		AuditID: auditID, AuditSHA256: append([]byte(nil), auditHash...), PreviousUUID: previousUUID,
	}, nil
}

func (witness *RekorWitness) verifyCertificateMetadata(uuid string, certificatePEM, auditHash []byte) (int64, string, error) {
	block, rest := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return 0, "", fmt.Errorf("%w: Rekor result %s does not carry an Oberth witness certificate", errForeignWitness, uuid)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return 0, "", fmt.Errorf("%w: parse Rekor result %s certificate: %w", errForeignWitness, uuid, err)
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return 0, "", fmt.Errorf("%w: Rekor result %s uses another key type", errForeignWitness, uuid)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil || !bytes.Equal(publicDER, witness.publicDER) {
		return 0, "", fmt.Errorf("%w: Rekor result %s belongs to another witness key", errForeignWitness, uuid)
	}
	if len(certificate.EmailAddresses) != 1 || certificate.EmailAddresses[0] != witness.identity ||
		certificate.Subject.CommonName != witness.identity || len(certificate.URIs) != 1 {
		return 0, "", fmt.Errorf("audit anchor: Rekor witness %s has invalid signed identity metadata", uuid)
	}
	if certificate.SignatureAlgorithm != x509.ECDSAWithSHA256 ||
		certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature) != nil {
		return 0, "", fmt.Errorf("audit anchor: Rekor witness %s certificate is not self-authenticated", uuid)
	}
	metadata := certificate.URIs[0]
	if metadata.Scheme != "https" || metadata.Host != witnessMetadataHost || metadata.RawQuery != "" || metadata.Fragment != "" {
		return 0, "", fmt.Errorf("audit anchor: Rekor witness %s has invalid signed chain metadata", uuid)
	}
	segments := strings.Split(strings.Trim(metadata.EscapedPath(), "/"), "/")
	if len(segments) != 4 || segments[0] != "v1" {
		return 0, "", fmt.Errorf("audit anchor: Rekor witness %s has invalid signed chain metadata", uuid)
	}
	auditID, err := strconv.ParseInt(segments[1], 10, 64)
	if err != nil || auditID < 0 || segments[2] != hex.EncodeToString(auditHash) {
		return 0, "", fmt.Errorf("audit anchor: Rekor witness %s metadata does not match its audit head", uuid)
	}
	previousUUID := ""
	if segments[3] != "root" {
		previousUUID, err = normalizedRekorUUID(segments[3])
		if err != nil || previousUUID != segments[3] {
			return 0, "", fmt.Errorf("audit anchor: Rekor witness %s has an invalid predecessor", uuid)
		}
	}
	return auditID, previousUUID, nil
}

type generatedRekorAPI struct {
	client *generatedclient.Rekor
}

func (api *generatedRekorAPI) SearchExact(ctx context.Context, certificatePEM, auditHash []byte) ([]string, error) {
	if len(certificatePEM) == 0 || len(auditHash) != sha256.Size {
		return nil, errors.New("exact Rekor search requires a witness certificate and SHA-256 audit head")
	}
	parameters := index.NewSearchIndexParamsWithContext(ctx)
	parameters.SetTimeout(rekorRequestTimeout)
	keyFormat := models.SearchIndexPublicKeyFormatX509
	parameters.Query = &models.SearchIndex{
		Hash: "sha256:" + hex.EncodeToString(auditHash), Operator: "and",
		PublicKey: &models.SearchIndexPublicKey{Content: strfmt.Base64(certificatePEM), Format: &keyFormat},
	}
	response, err := api.client.Index.SearchIndex(parameters)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), response.Payload...), nil
}

func (api *generatedRekorAPI) SearchIdentity(ctx context.Context, identity string) ([]string, error) {
	if strings.TrimSpace(identity) == "" || strings.ContainsAny(identity, "\x00\r\n") {
		return nil, errors.New("identity Rekor search requires a clean witness identity")
	}
	parameters := index.NewSearchIndexParamsWithContext(ctx)
	parameters.SetTimeout(rekorRequestTimeout)
	parameters.Query = &models.SearchIndex{Email: strfmt.Email(identity)}
	response, err := api.client.Index.SearchIndex(parameters)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), response.Payload...), nil
}

func (api *generatedRekorAPI) Get(ctx context.Context, uuid string) (string, models.LogEntryAnon, error) {
	parameters := entries.NewGetLogEntryByUUIDParamsWithContext(ctx)
	parameters.SetTimeout(rekorRequestTimeout)
	parameters.EntryUUID = uuid
	response, err := api.client.Entries.GetLogEntryByUUID(parameters)
	if err != nil {
		return "", models.LogEntryAnon{}, err
	}
	if len(response.Payload) != 1 {
		return "", models.LogEntryAnon{}, fmt.Errorf("rekor returned %d entries for UUID %s", len(response.Payload), uuid)
	}
	for actualUUID, entry := range response.Payload {
		requestedBase, requestedErr := sharding.GetUUIDFromIDString(uuid)
		actualBase, actualErr := sharding.GetUUIDFromIDString(actualUUID)
		if requestedErr != nil || actualErr != nil || requestedBase != actualBase {
			return "", models.LogEntryAnon{}, fmt.Errorf("rekor returned UUID %s for request %s", actualUUID, uuid)
		}
		return actualUUID, entry, nil
	}
	return "", models.LogEntryAnon{}, errors.New("rekor entry is missing")
}

func (api *generatedRekorAPI) Create(ctx context.Context, proposed models.ProposedEntry) (string, models.LogEntryAnon, error) {
	parameters := entries.NewCreateLogEntryParamsWithContext(ctx)
	parameters.SetTimeout(rekorRequestTimeout)
	parameters.SetProposedEntry(proposed)
	response, err := api.client.Entries.CreateLogEntry(parameters)
	if err != nil {
		var conflict *entries.CreateLogEntryConflict
		if errors.As(err, &conflict) && conflict.Location != "" {
			location, parseErr := url.Parse(string(conflict.Location))
			if parseErr == nil && path.Base(location.Path) != "." && path.Base(location.Path) != "/" {
				return api.Get(ctx, path.Base(location.Path))
			}
		}
		return "", models.LogEntryAnon{}, err
	}
	if len(response.Payload) != 1 {
		return "", models.LogEntryAnon{}, fmt.Errorf("rekor created %d entries", len(response.Payload))
	}
	for uuid := range response.Payload {
		return api.Get(ctx, uuid)
	}
	return "", models.LogEntryAnon{}, errors.New("rekor create response is missing")
}

type tufRekorVerifier struct {
	keys map[string]signature.Verifier
}

func loadTUFRekorVerifier(ctx context.Context) (*tufRekorVerifier, error) {
	client, err := tuf.NewFromEnv(ctx)
	if err != nil {
		return nil, err
	}
	targets, err := client.GetTargetsByMeta(tuf.Rekor, []string{"rekor.pub"})
	if err != nil {
		return nil, err
	}
	verifiers := make(map[string]signature.Verifier, len(targets))
	for _, target := range targets {
		if target.Status != tuf.Active && target.Status != tuf.Expired {
			continue
		}
		publicKey, err := cryptoutils.UnmarshalPEMToPublicKey(target.Target)
		if err != nil {
			return nil, fmt.Errorf("parse TUF Rekor key %s: %w", target.Name, err)
		}
		publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
		if err != nil {
			return nil, fmt.Errorf("encode TUF Rekor key %s: %w", target.Name, err)
		}
		logID := sha256.Sum256(publicDER)
		verifier, err := signature.LoadVerifier(publicKey, crypto.SHA256)
		if err != nil {
			return nil, fmt.Errorf("load TUF Rekor verifier %s: %w", target.Name, err)
		}
		verifiers[hex.EncodeToString(logID[:])] = verifier
	}
	if len(verifiers) == 0 {
		return nil, errors.New("TUF metadata contains no trusted Rekor keys")
	}
	return &tufRekorVerifier{keys: verifiers}, nil
}

func (verifier *tufRekorVerifier) Verify(ctx context.Context, entry *models.LogEntryAnon) error {
	if entry == nil || entry.LogID == nil {
		return errors.New("rekor entry has no log ID")
	}
	key, ok := verifier.keys[strings.ToLower(*entry.LogID)]
	if !ok {
		return fmt.Errorf("rekor log ID %s is absent from TUF trust metadata", *entry.LogID)
	}
	return rekorverify.VerifyLogEntry(ctx, entry, key)
}
