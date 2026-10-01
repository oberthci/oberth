//
// Copyright 2021 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auditanchor

// This file is a minimal, behavior-identical port of the Rekor entry-ID
// validators from github.com/sigstore/rekor/pkg/sharding (sharding.go at
// v1.5.3, Apache-2.0; copyright header retained above). Oberth needs exactly
// one function from that package — GetUUIDFromIDString — but importing it
// links Rekor's SERVER-side pkg/signer and pkg/trillianclient and, through
// them, the AWS, GCP, and Azure KMS SDK closure (aws-sdk-go-v2,
// cloud.google.com/go/kms, the Azure SDK, gRPC, trillian) into a binary that
// only ever acts as a Rekor client. Porting these pure string-validation
// functions removes that entire tree from the module graph and the binary.
// rekoruuid_test.go pins parity with the upstream sharding_test.go vectors.

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// A Rekor EntryID is a hex-encoded TreeID (16 characters, an int64 naming
// the trillian shard) followed by a hex-encoded UUID (64 characters, the
// entry's merkle leaf hash). A bare UUID is the 64-character form alone.
const (
	rekorTreeIDHexStringLen  = 16
	rekorUUIDHexStringLen    = 64
	rekorEntryIDHexStringLen = rekorTreeIDHexStringLen + rekorUUIDHexStringLen
)

// errRekorZeroTreeID mirrors upstream's "0 is not a valid TreeID" condition,
// which rekorUUIDFromIDString deliberately tolerates: an EntryID whose TreeID
// is zero still carries a well-formed UUID worth extracting.
var errRekorZeroTreeID = errors.New("0 is not a valid TreeID")

// rekorUUIDFromIDString returns the UUID (with no prepended TreeID) from a
// UUID or EntryID string, validating the UUID and, when present, the TreeID.
// Port of sharding.GetUUIDFromIDString.
func rekorUUIDFromIDString(id string) (string, error) {
	switch len(id) {
	case rekorUUIDHexStringLen:
		if err := rekorValidateUUID(id); err != nil {
			return "", err
		}
		return id, nil
	case rekorEntryIDHexStringLen:
		if err := rekorValidateEntryID(id); err != nil {
			if errors.Is(err, errRekorZeroTreeID) {
				return id[len(id)-rekorUUIDHexStringLen:], nil
			}
			return "", err
		}
		return id[len(id)-rekorUUIDHexStringLen:], nil
	default:
		return "", fmt.Errorf("invalid ID len %v for %v", len(id), id)
	}
}

// rekorValidateUUID checks the UUID part of a UUID or EntryID string.
// Port of sharding.ValidateUUID.
func rekorValidateUUID(u string) error {
	switch len(u) {
	case rekorEntryIDHexStringLen:
		return rekorValidateUUID(u[len(u)-rekorUUIDHexStringLen:])
	case rekorUUIDHexStringLen:
		if _, err := hex.DecodeString(u); err != nil {
			return fmt.Errorf("id %v is not a valid hex string: %w", u, err)
		}
		return nil
	default:
		return fmt.Errorf("invalid ID len %v for %v", len(u), u)
	}
}

// rekorValidateTreeID checks the TreeID part of a TreeID or EntryID string:
// it must be a hex-encoded int64 and must not be zero.
// Port of sharding.ValidateTreeID.
func rekorValidateTreeID(t string) error {
	switch len(t) {
	case rekorEntryIDHexStringLen:
		return rekorValidateTreeID(t[:rekorTreeIDHexStringLen])
	case rekorTreeIDHexStringLen:
		i, err := strconv.ParseInt(t, 16, 64)
		if err != nil {
			return fmt.Errorf("could not convert treeID %v to int64: %w", t, err)
		}
		if i == 0 {
			return errRekorZeroTreeID
		}
		return nil
	default:
		return fmt.Errorf("TreeID len expected to be %v but got %v", rekorTreeIDHexStringLen, len(t))
	}
}

// rekorValidateEntryID checks a full EntryID — the UUID first, then the
// TreeID, preserving upstream's evaluation order so the zero-TreeID
// tolerance in rekorUUIDFromIDString never rescues a malformed UUID.
// Port of sharding.ValidateEntryID.
func rekorValidateEntryID(id string) error {
	if err := rekorValidateUUID(id); err != nil {
		return err
	}
	return rekorValidateTreeID(id)
}
