package reponame

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestRepositoryDiagnosticsAreShortWithFullValueContext(t *testing.T) {
	for _, component := range []string{"", "upstream", "organization", "repository path", "repository name"} {
		for _, reserved := range []bool{false, true} {
			e := &InvalidNameError{Component: component, Value: strings.Repeat("value", 100), reserved: reserved}
			for text, limit := range map[string]int{e.Reason(): 80, e.Remedy(): 120} {
				if err := publicerr.CheckMessage(text, limit); err != nil {
					t.Fatal(err, text)
				}
			}
			if !strings.Contains(e.Error(), e.Value) {
				t.Fatal("full input context lost")
			}
		}
	}
}
