package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/client"
)

type remedyLostReply struct{}

func (remedyLostReply) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("injected lost reply")
}

func remedyOutcomeTexts(t *testing.T) []remedyText {
	t.Helper()
	c, err := client.New(client.Config{
		Options: client.Options{URL: "https://oberth.ci"}, Transport: remedyLostReply{},
		Now:  func() time.Time { return time.Unix(0, 0) },
		Wait: func(context.Context, time.Duration) error { return errors.New("unexpected retry") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	origin := remedyFunctionOrigin(t, filepath.Join("..", "..", "internal", "client", "failures.go"), "uncertain")
	var result []remedyText
	seen := map[string]bool{}
	for _, row := range commands() {
		if !row.definition.Mutation || row.definition.Check == "" {
			continue
		}
		_, _, err := c.Call(t.Context(), row.definition, json.RawMessage(`{}`))
		if !client.IsOutcomeUnknown(err) {
			t.Fatalf("%s: expected a rendered uncertain outcome, got %v", row.definition.Name, err)
		}
		text := err.Error()
		if !seen[text] {
			result = append(result, remedyText{"internal/client/failures.go", origin, text, ""})
			seen[text] = true
		}
	}
	return result
}
