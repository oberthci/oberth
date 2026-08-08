package auditanchor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/rekor/pkg/generated/models"
	hashedrekordv001 "github.com/sigstore/rekor/pkg/types/hashedrekord/v0.0.1"

	"github.com/oberthci/oberth/internal/model"
)

type memoryRekorAPI struct {
	entries           map[string]models.LogEntryAnon
	byExact           map[string][]string
	byEmail           map[string][]string
	searchErr         error
	searchIdentityErr error
	getErr            error
	createErr         error
	getCalls          int
	now               time.Time
}

func newMemoryRekorAPI() *memoryRekorAPI {
	return &memoryRekorAPI{
		entries: make(map[string]models.LogEntryAnon), byExact: make(map[string][]string),
		byEmail: make(map[string][]string),
		now:     time.Date(2026, 8, 6, 19, 0, 0, 0, time.UTC),
	}
}

func (api *memoryRekorAPI) SearchExact(_ context.Context, certificatePEM, auditHash []byte) ([]string, error) {
	if api.searchErr != nil {
		return nil, api.searchErr
	}
	return append([]string(nil), api.byExact[exactSearchKey(certificatePEM, auditHash)]...), nil
}

func (api *memoryRekorAPI) SearchIdentity(_ context.Context, identity string) ([]string, error) {
	if api.searchIdentityErr != nil {
		return nil, api.searchIdentityErr
	}
	return append([]string(nil), api.byEmail[identity]...), nil
}

func exactSearchKey(certificatePEM, auditHash []byte) string {
	return string(certificatePEM) + "\x00" + string(auditHash)
}

func (api *memoryRekorAPI) Get(_ context.Context, uuid string) (string, models.LogEntryAnon, error) {
	api.getCalls++
	if api.getErr != nil {
		return "", models.LogEntryAnon{}, api.getErr
	}
	entry, ok := api.entries[uuid]
	if !ok {
		return "", models.LogEntryAnon{}, errors.New("not found")
	}
	return uuid, entry, nil
}

func (api *memoryRekorAPI) Create(_ context.Context, proposed models.ProposedEntry) (string, models.LogEntryAnon, error) {
	if api.createErr != nil {
		return "", models.LogEntryAnon{}, api.createErr
	}
	body, err := json.Marshal(proposed)
	if err != nil {
		return "", models.LogEntryAnon{}, err
	}
	rekord, ok := proposed.(*models.Hashedrekord)
	if !ok {
		return "", models.LogEntryAnon{}, fmt.Errorf("unexpected proposal %T", proposed)
	}
	var schema models.HashedrekordV001Schema
	if err := hashedrekordv001.DecodeEntry(rekord.Spec, &schema); err != nil {
		return "", models.LogEntryAnon{}, err
	}
	digest := sha256.Sum256(body)
	uuid := hex.EncodeToString(digest[:])
	logIndex := int64(len(api.entries))
	integratedAt := api.now.Add(time.Duration(logIndex) * time.Second).Unix()
	logID := string(bytes.Repeat([]byte{'a'}, 64))
	checkpoint := "fake signed checkpoint"
	rootHash := string(bytes.Repeat([]byte{'b'}, 64))
	treeSize := logIndex + 1
	entry := models.LogEntryAnon{
		Body: base64.StdEncoding.EncodeToString(body), IntegratedTime: &integratedAt, LogID: &logID, LogIndex: &logIndex,
		Verification: &models.LogEntryAnonVerification{
			InclusionProof: &models.InclusionProof{
				Checkpoint: &checkpoint, Hashes: []string{}, LogIndex: &logIndex, RootHash: &rootHash, TreeSize: &treeSize,
			},
			SignedEntryTimestamp: []byte("fake SET"),
		},
	}
	api.entries[uuid] = entry
	certificatePEM := schema.Signature.PublicKey.Content
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", models.LogEntryAnon{}, errors.New("proposal has no witness certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || len(certificate.EmailAddresses) != 1 {
		return "", models.LogEntryAnon{}, errors.New("proposal has no witness identity")
	}
	auditHash, err := hex.DecodeString(*schema.Data.Hash.Value)
	if err != nil {
		return "", models.LogEntryAnon{}, err
	}
	api.byExact[exactSearchKey(certificatePEM, auditHash)] = append(api.byExact[exactSearchKey(certificatePEM, auditHash)], uuid)
	email := certificate.EmailAddresses[0]
	api.byEmail[email] = append(api.byEmail[email], uuid)
	return uuid, entry, nil
}

type fakeRekorEntryVerifier struct {
	calls int
	err   error
}

func (verifier *fakeRekorEntryVerifier) Verify(context.Context, *models.LogEntryAnon) error {
	verifier.calls++
	return verifier.err
}

func TestRekorWitnessPublishesAndRecoversExactHistoryAfterRestart(t *testing.T) {
	ctx := context.Background()
	api := newMemoryRekorAPI()
	verifier := &fakeRekorEntryVerifier{}
	keyMaterial := []byte("persistent SSH host private key bytes")
	witness, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	firstHash := sha256.Sum256([]byte("first audit head"))
	secondHash := sha256.Sum256([]byte("second audit head"))
	var publishedHistory []model.AuditWitness
	for index, hash := range [][sha256.Size]byte{firstHash, secondHash} {
		head := model.AuditHead{ID: int64(index + 1), SHA256: hash[:]}
		if index == 0 {
			if _, err := witness.History(ctx, nil, head); err != nil {
				t.Fatal(err)
			}
		}
		published, err := witness.Publish(ctx, head)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(published.AuditSHA256, hash[:]) || published.UUID == "" {
			t.Fatalf("published witness = %#v", published)
		}
		publishedHistory = append(publishedHistory, published)
	}

	restarted, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	history, err := restarted.History(ctx, publishedHistory, model.AuditHead{ID: 2, SHA256: secondHash[:]})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || !bytes.Equal(history[0].AuditSHA256, firstHash[:]) || !bytes.Equal(history[1].AuditSHA256, secondHash[:]) {
		t.Fatalf("recovered history = %#v", history)
	}
	otherIdentity, err := newRekorWitness(api, verifier, []byte("rotated host key"))
	if err != nil {
		t.Fatal(err)
	}
	otherHistory, err := otherIdentity.History(ctx, nil, model.AuditHead{ID: 2, SHA256: secondHash[:]})
	if err != nil || len(otherHistory) != 0 {
		t.Fatalf("other identity history = %#v, %v", otherHistory, err)
	}
	if verifier.calls < 4 {
		t.Fatalf("transparency verifier calls = %d, want publication and recovery verification", verifier.calls)
	}
}

func TestRekorWitnessRejectsTamperedEntryAndClassifiesTransportFailure(t *testing.T) {
	ctx := context.Background()
	api := newMemoryRekorAPI()
	verifier := &fakeRekorEntryVerifier{}
	witness, err := newRekorWitness(api, verifier, []byte("persistent host key"))
	if err != nil {
		t.Fatal(err)
	}
	headHash := sha256.Sum256([]byte("audit head"))
	head := model.AuditHead{ID: 1, SHA256: headHash[:]}
	if _, err := witness.History(ctx, nil, head); err != nil {
		t.Fatal(err)
	}
	published, err := witness.Publish(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := newRekorWitness(api, verifier, []byte("persistent host key"))
	if err != nil {
		t.Fatal(err)
	}
	entry := api.entries[published.UUID]
	body, err := base64.StdEncoding.DecodeString(entry.Body.(string))
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	spec := envelope["spec"].(map[string]any)
	data := spec["data"].(map[string]any)
	hash := data["hash"].(map[string]any)
	hash["value"] = string(bytes.Repeat([]byte{'0'}, sha256.Size*2))
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	entry.Body = base64.StdEncoding.EncodeToString(tampered)
	api.entries[published.UUID] = entry
	if _, err := reader.History(ctx, []model.AuditWitness{published}, head); err == nil {
		t.Fatal("tampered external witness history verified")
	}

	api.searchErr = errors.New("network unavailable")
	freshReader, err := newRekorWitness(api, verifier, []byte("persistent host key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := freshReader.History(ctx, nil, head); !errors.Is(err, ErrWitnessUnavailable) {
		t.Fatalf("history transport error = %v, want ErrWitnessUnavailable", err)
	}
}

func TestRekorWitnessReverifiesEveryPinnedEntryAndRejectsOmittedTip(t *testing.T) {
	ctx := context.Background()
	api := newMemoryRekorAPI()
	verifier := &fakeRekorEntryVerifier{}
	keyMaterial := []byte("persistent host key")
	publisher, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	var published []model.AuditWitness
	var finalHead model.AuditHead
	for index, value := range []string{"first", "second"} {
		hash := sha256.Sum256([]byte(value))
		finalHead = model.AuditHead{ID: int64(index + 1), SHA256: hash[:]}
		if index == 0 {
			if _, err := publisher.History(ctx, nil, finalHead); err != nil {
				t.Fatal(err)
			}
		}
		entry, err := publisher.Publish(ctx, finalHead)
		if err != nil {
			t.Fatal(err)
		}
		published = append(published, entry)
	}

	reader, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	first, err := reader.History(ctx, published, finalHead)
	if err != nil {
		t.Fatal(err)
	}
	getCalls := api.getCalls
	verifyCalls := verifier.calls
	second, err := reader.History(ctx, published, finalHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 || api.getCalls != getCalls+2 || verifier.calls != verifyCalls+2 {
		t.Fatalf(
			"reverified history lengths/get calls/verify calls = %d/%d/%d/%d, want 2/2/%d/%d",
			len(first), len(second), api.getCalls, verifier.calls, getCalls+2, verifyCalls+2,
		)
	}

	getCalls = api.getCalls
	api.getErr = errors.New("network unavailable")
	if _, err := reader.History(ctx, published, finalHead); !errors.Is(err, ErrWitnessUnavailable) {
		t.Fatalf("cached history accepted unavailable pinned Rekor entries: %v", err)
	}
	if api.getCalls != getCalls+1 {
		t.Fatalf("unavailable history GET calls = %d, want %d", api.getCalls, getCalls+1)
	}

	api.getErr = nil
	if _, err := reader.History(ctx, published[:1], model.AuditHead{ID: published[0].AuditID, SHA256: published[0].AuditSHA256}); err == nil || !strings.Contains(err.Error(), "omitted previously verified witness") {
		t.Fatalf("omitted cached witness error = %v", err)
	}
}

func TestRekorWitnessRecoversOmittedPredecessorAfterFreshRestart(t *testing.T) {
	ctx := context.Background()
	api := newMemoryRekorAPI()
	verifier := &fakeRekorEntryVerifier{}
	keyMaterial := []byte("persistent host key")
	publisher, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	var published []model.AuditWitness
	for index, value := range []string{"first", "second"} {
		hash := sha256.Sum256([]byte(value))
		head := model.AuditHead{ID: int64(index + 1), SHA256: hash[:]}
		if index == 0 {
			if _, err := publisher.History(ctx, nil, head); err != nil {
				t.Fatal(err)
			}
		}
		entry, err := publisher.Publish(ctx, head)
		if err != nil {
			t.Fatal(err)
		}
		published = append(published, entry)
	}

	// The index returns only the newest member. Its immutable predecessor link
	// must make a new process fetch and verify the omitted older entry directly.
	restarted, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	history, err := restarted.History(ctx, published, model.AuditHead{ID: 2, SHA256: published[1].AuditSHA256})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].UUID != published[0].UUID || history[1].UUID != published[1].UUID {
		t.Fatalf("linked recovery history = %#v", history)
	}

	delete(api.entries, published[0].UUID)
	anotherRestart, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anotherRestart.History(ctx, published, model.AuditHead{ID: 2, SHA256: published[1].AuditSHA256}); err == nil || !strings.Contains(err.Error(), "retrieve pinned Rekor witness") {
		t.Fatalf("missing linked predecessor error = %v", err)
	}
}

func TestRekorWitnessRecoversOnlyExactUnpinnedPublication(t *testing.T) {
	ctx := context.Background()
	api := newMemoryRekorAPI()
	verifier := &fakeRekorEntryVerifier{}
	keyMaterial := []byte("persistent host key")
	publisher, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	firstHash := sha256.Sum256([]byte("first"))
	firstHead := model.AuditHead{ID: 1, SHA256: firstHash[:]}
	if _, err := publisher.History(ctx, nil, firstHead); err != nil {
		t.Fatal(err)
	}
	first, err := publisher.Publish(ctx, firstHead)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after Rekor accepted the root but before Kubernetes
	// continuity was created. A fresh process reconstructs the exact
	// certificate+hash query and recovers that one immutable entry.
	restarted, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.History(ctx, nil, firstHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || !sameAuditWitness(recovered[0], first) {
		t.Fatalf("recovered root = %#v, want %#v", recovered, first)
	}

	secondHash := sha256.Sum256([]byte("second"))
	secondHead := model.AuditHead{ID: 2, SHA256: secondHash[:]}
	second, err := publisher.Publish(ctx, secondHead)
	if err != nil {
		t.Fatal(err)
	}
	afterSecondCrash, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err = afterSecondCrash.History(ctx, []model.AuditWitness{first}, secondHead)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 2 || !sameAuditWitness(recovered[1], second) {
		t.Fatalf("recovered suffix = %#v, want second %#v", recovered, second)
	}
}

func TestHistoryRecoverySkipsForeignGenesisCollisionsInSharedLog(t *testing.T) {
	ctx := context.Background()
	api := newMemoryRekorAPI()
	verifier := &fakeRekorEntryVerifier{}
	genesisHead := model.AuditHead{ID: 0, SHA256: make([]byte, sha256.Size)}

	// Another deployment's witness identity has already committed the identical
	// deterministic empty-chain genesis head to the shared transparency log.
	other, err := newRekorWitness(api, verifier, []byte("someone else's host key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.History(ctx, nil, genesisHead); err != nil {
		t.Fatal(err)
	}
	foreignRoot, err := other.Publish(ctx, genesisHead)
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := newRekorWitness(api, verifier, []byte("brand new host key"))
	if err != nil {
		t.Fatal(err)
	}
	// The production index also matches on the key-independent genesis
	// metadata, returning the other identity's entry for our exact search.
	freshCertificate, err := fresh.certificatePEM(genesisHead, "")
	if err != nil {
		t.Fatal(err)
	}
	collisionKey := exactSearchKey(freshCertificate, genesisHead.SHA256)
	api.byExact[collisionKey] = append(api.byExact[collisionKey], foreignRoot.UUID)

	history, err := fresh.History(ctx, nil, genesisHead)
	if err != nil {
		t.Fatalf("fresh install with a foreign genesis collision failed recovery: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("foreign genesis collision entered recovered history: %#v", history)
	}
	ownRoot, err := fresh.Publish(ctx, genesisHead)
	if err != nil {
		t.Fatalf("publish after skipping a foreign collision: %v", err)
	}
	if ownRoot.UUID == foreignRoot.UUID {
		t.Fatal("own genesis witness deduplicated into a foreign identity's entry")
	}

	// After our own publication, recovery must find exactly our entry among
	// the colliding foreign ones, in any order.
	api.byExact[collisionKey] = append([]string{foreignRoot.UUID}, api.byExact[collisionKey]...)
	restarted, err := newRekorWitness(api, verifier, []byte("brand new host key"))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.History(ctx, nil, genesisHead)
	if err != nil {
		t.Fatalf("recovery among foreign collisions: %v", err)
	}
	if len(recovered) != 1 || !sameAuditWitness(recovered[0], ownRoot) {
		t.Fatalf("recovered = %#v, want exactly own root %#v", recovered, ownRoot)
	}
}

func TestAbandonedChainTipReturnsLatestOwnWitnessAndSkipsForeignEntries(t *testing.T) {
	ctx := context.Background()
	api := newMemoryRekorAPI()
	verifier := &fakeRekorEntryVerifier{}
	keyMaterial := []byte("persistent host key")
	publisher, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	var published []model.AuditWitness
	for index, value := range []string{"first", "second", "third"} {
		hash := sha256.Sum256([]byte(value))
		head := model.AuditHead{ID: int64(index + 1), SHA256: hash[:]}
		if index == 0 {
			if _, err := publisher.History(ctx, nil, head); err != nil {
				t.Fatal(err)
			}
		}
		entry, err := publisher.Publish(ctx, head)
		if err != nil {
			t.Fatal(err)
		}
		published = append(published, entry)
	}

	// A third party can publish arbitrary entries indexed under the same email
	// identity: another key's genuine witness and a non-hashedrekord body.
	foreign, err := newRekorWitness(api, verifier, []byte("another host key"))
	if err != nil {
		t.Fatal(err)
	}
	foreignHash := sha256.Sum256([]byte("foreign head"))
	foreignHead := model.AuditHead{ID: 9, SHA256: foreignHash[:]}
	if _, err := foreign.History(ctx, nil, foreignHead); err != nil {
		t.Fatal(err)
	}
	foreignEntry, err := foreign.Publish(ctx, foreignHead)
	if err != nil {
		t.Fatal(err)
	}
	api.byEmail[publisher.identity] = append(api.byEmail[publisher.identity], foreignEntry.UUID)
	malformedUUID := strings.Repeat("f", 64)
	malformedIndex := int64(4096)
	malformedTime := api.now.Unix()
	logID := strings.Repeat("a", 64)
	checkpoint := "fake signed checkpoint"
	rootHash := strings.Repeat("b", 64)
	treeSize := malformedIndex + 1
	api.entries[malformedUUID] = models.LogEntryAnon{
		Body: "!!!not-base64!!!", IntegratedTime: &malformedTime, LogID: &logID, LogIndex: &malformedIndex,
		Verification: &models.LogEntryAnonVerification{
			InclusionProof: &models.InclusionProof{
				Checkpoint: &checkpoint, Hashes: []string{}, LogIndex: &malformedIndex, RootHash: &rootHash, TreeSize: &treeSize,
			},
			SignedEntryTimestamp: []byte("fake SET"),
		},
	}
	api.byEmail[publisher.identity] = append(api.byEmail[publisher.identity], malformedUUID)

	fresh, err := newRekorWitness(api, verifier, keyMaterial)
	if err != nil {
		t.Fatal(err)
	}
	tip, found, err := fresh.AbandonedChainTip(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !found || !sameAuditWitness(tip, published[2]) {
		t.Fatalf("abandoned tip = %#v found=%v, want latest own witness %#v", tip, found, published[2])
	}

	// The enumeration must not poison the recovery cache: full history recovery
	// and publication on the same instance must still succeed afterwards.
	history, err := fresh.History(ctx, published, model.AuditHead{ID: 3, SHA256: published[2].AuditSHA256})
	if err != nil || len(history) != 3 {
		t.Fatalf("recovery after tip enumeration = %#v, %v", history, err)
	}
}

func TestAbandonedChainTipReportsAbsentHistoryAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	api := newMemoryRekorAPI()
	verifier := &fakeRekorEntryVerifier{}
	witness, err := newRekorWitness(api, verifier, []byte("never published host key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := witness.AbandonedChainTip(ctx); err != nil || found {
		t.Fatalf("clean identity tip: found=%v err=%v, want absent history", found, err)
	}

	api.searchIdentityErr = errors.New("index unavailable")
	if _, _, err := witness.AbandonedChainTip(ctx); !errors.Is(err, ErrWitnessUnavailable) {
		t.Fatalf("identity search transport error = %v, want ErrWitnessUnavailable", err)
	}
	api.searchIdentityErr = nil

	for index := range maximumIdentitySearchResults + 1 {
		api.byEmail[witness.identity] = append(api.byEmail[witness.identity], fmt.Sprintf("%064x", index))
	}
	if _, _, err := witness.AbandonedChainTip(ctx); err == nil || !strings.Contains(err.Error(), "more than the supported") {
		t.Fatalf("identity flood error = %v, want bounded enumeration", err)
	}
}

func TestNormalizeRekorUUIDReducesShardQualifiedIdentifiers(t *testing.T) {
	base := strings.Repeat("c", 64)
	sharded := strings.Repeat("1", 16) + base
	normalized, err := NormalizeRekorUUID(sharded)
	if err != nil || normalized != base {
		t.Fatalf("normalized = %q, %v, want %q", normalized, err, base)
	}
	if _, err := NormalizeRekorUUID("zz"); err == nil {
		t.Fatal("malformed UUID normalized")
	}
}

func TestWitnessCertificateIsDeterministicForExactRecovery(t *testing.T) {
	witness, err := newRekorWitness(newMemoryRekorAPI(), &fakeRekorEntryVerifier{}, []byte("persistent host key"))
	if err != nil {
		t.Fatal(err)
	}
	headHash := sha256.Sum256([]byte("audit head"))
	head := model.AuditHead{ID: 42, SHA256: headHash[:]}
	first, err := witness.certificatePEM(head, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := witness.certificatePEM(head, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("same witness intent produced a different exact-search certificate")
	}
}

func TestWitnessKeyDerivationIsStableAndDomainSeparated(t *testing.T) {
	first, err := deriveWitnessKey([]byte("host-key-one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := deriveWitnessKey([]byte("host-key-one"))
	if err != nil {
		t.Fatal(err)
	}
	different, err := deriveWitnessKey([]byte("host-key-two"))
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := first.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := second.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	differentBytes, err := different.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBytes, secondBytes) || bytes.Equal(firstBytes, differentBytes) {
		t.Fatal("witness key derivation is not stable and identity-specific")
	}
	if _, err := deriveWitnessKey(nil); err == nil {
		t.Fatal("empty host key material derived a witness identity")
	}
}
