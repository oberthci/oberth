package model

import (
	"github.com/oberthci/oberth/internal/actor"
)

// CIRequest carries an attributed ref update; release authority is decided by admission.
type CIRequest struct {
	ReceiveID     string      `json:"receive_id"`
	Repo          string      `json:"repo"`
	Ref           string      `json:"ref"`
	PreviousSHA   string      `json:"previous_sha"`
	ObjectSHA     string      `json:"object_sha"`
	CommitSHA     *string     `json:"commit_sha"`
	Actor         actor.Actor `json:"actor"`
	Trigger       RunTrigger  `json:"trigger"`
	ScheduleEntry string      `json:"schedule_entry"`
	Assist        bool        `json:"assist"`
	RequestedFile string      `json:"requested_file,omitempty"`
}

func (v CIRequest) Clone() CIRequest {
	v.CommitSHA = copyPointer(v.CommitSHA)
	v.Actor = v.Actor.Clone()
	return v
}
