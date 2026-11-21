package main

import (
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"testing"
)

func TestLiteralComponentMessageCatalogue(t *testing.T) {
	for _, path := range []string{"gitcache/messages.go", "gitssh/config.go", "sourcedelivery/messages.go", "sourceplugin/messages.go", "sshserver/command.go"} {
		t.Run(path, func(t *testing.T) { gittest.Messages(t, "../../internal/"+path) })
	}
}
