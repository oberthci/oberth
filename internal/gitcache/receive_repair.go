package gitcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// Repair preserves a recoverable native event ID. With no such identity,
// inspection names the exact damaged bytes and grants no retention ownership.
func readReceiveRepair(store, repo string) (Reservation, string, error) {
	r, err := readReservation(store)
	var corrupt *CorruptReceiveError
	if !errors.As(err, &corrupt) {
		return r, r.Event.ID, err
	}
	data, readErr := readReservationData(store)
	if readErr != nil {
		return Reservation{}, "", readErr
	}
	if len(data) > maxReceiveRecordBytes {
		return Reservation{}, "", err
	}
	r = receiveRepairFacts(data)
	if r.Event.Repository != "" && r.Event.Repository != repo {
		r = Reservation{}
	}
	id, owner := r.Event.ID, r.Event.ID
	if !validReceiveID(id) {
		id, owner = fmt.Sprintf("corrupt-%x", sha256.Sum256(data)), ""
	}
	r.State = "corrupt"
	r.Event = ReceiveEvent{Repository: repo, ID: id}
	return r, owner, nil
}

// Decode complete fields before the damaged suffix using the standard decoder.
// Incomplete or invalid predecessor maps carry no authority to rewrite refs.
func receiveRepairFacts(data []byte) Reservation {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return Reservation{}
	}
	var record Reservation
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			break
		}
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			break
		}
		switch key {
		case "before":
			record.Before = repairPredecessors(value)
		case "event":
			var event ReceiveEvent
			if json.Unmarshal(value, &event) == nil {
				record.Event = event
			}
		}
	}
	return record
}

func repairPredecessors(data []byte) map[string]string {
	var before map[string]string
	if json.Unmarshal(data, &before) != nil {
		return nil
	}
	for ref, oid := range before {
		if sourcecontract.ValidatePublicRef(ref) != nil || !gitoid.Valid(oid) {
			return nil
		}
	}
	return before
}
