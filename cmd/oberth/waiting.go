package main

import (
	"encoding/json"
	"fmt"

	"github.com/oberthci/oberth/internal/apicontract"
)

func waitingForArgs(d apicontract.Definition, args any) string {
	input, _ := json.Marshal(args)
	return waitingFor(d, input)
}
func waitingFor(d apicontract.Definition, input json.RawMessage) string {
	var args map[string]any
	_ = json.Unmarshal(input, &args)
	_, seconds := apicontract.TimeoutSeconds(d, input)
	target := cliWords(d.Name)
	if d.Name == "run_wait" {
		target = "run " + publicText(args["id"])
		state := publicText(args["until"])
		if state == "" {
			state = "finished"
		}
		target += " to reach " + state
	} else if d.Name == "wait" {
		switch {
		case publicText(args["sha"]) != "":
			target = "CI at " + publicText(args["sha"])
		case publicText(args["ref"]) != "":
			target = "the release run of " + publicText(args["ref"])
		default:
			target = "the selected run"
		}
	} else if d.Name == "events_wait" {
		target = eventWaitSubject(args)
	} else if d.Name == "promote_status" {
		target = "promotion " + publicText(args["id"]) + " to finish"
	} else if id := publicText(args["id"]); id != "" {
		target += " " + id
	}
	return fmt.Sprintf("Waiting up to %ds for %s.", seconds, target)
}

func eventWaitSubject(args map[string]any) string {
	kinds, _ := args["kinds"].([]any)
	for _, kind := range kinds {
		if kind == "release.published" {
			return "the publication receipt"
		}
	}
	return "new authorized events"
}
