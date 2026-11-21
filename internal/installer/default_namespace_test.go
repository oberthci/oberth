package installer_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestChartAndGoDefaultNamespaceAgree(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartPath(), "templates", "_server.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`ne \$root\.Release\.Namespace "([^"]+)"`).FindSubmatch(raw)
	if len(match) != 2 || string(match[1]) != installer.Defaults().Namespace {
		t.Fatal("chart cluster names and Go default namespace differ", string(raw))
	}
}
