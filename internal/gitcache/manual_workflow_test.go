package gitcache_test

import (
	"github.com/oberthci/oberth/internal/gitcache"
	"testing"
)

func TestManualWorkflowPushOptions(t *testing.T) {
	for _, options := range [][]string{
		{"oberth.run=1", "oberth.file=.oberth/walk.yaml"},
		{"oberth.file=hack/walk-vm.yaml", "oberth.assist=1", "oberth.run=1"},
	} {
		p, err := gitcache.ParsePushOptions(options)
		if err != nil || p.File == "" || !p.Run {
			t.Fatal("explicit workflow refused", p, err)
		}
	}
	for _, options := range [][]string{
		{"oberth.file=.oberth/walk.yaml"},
		{"oberth.run=0", "oberth.file=.oberth/walk.yaml"},
		{"oberth.run=1", "oberth.file="},
		{"oberth.run=1", "oberth.file=../walk.yaml"},
		{"oberth.run=1", "oberth.file=/walk.yaml"},
		{"oberth.run=1", "oberth.file=.git/config"},
		{"oberth.run=1", "oberth.file=walk.yaml", "oberth.file=walk.yaml"},
	} {
		if _, err := gitcache.ParsePushOptions(options); err == nil {
			t.Fatal("invalid workflow option accepted", options)
		}
	}
}
