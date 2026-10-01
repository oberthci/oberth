package goproxy

import (
	"errors"
	"path"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
)

// NamespaceConfig is administrator-owned namespace authority. Repository code
// cannot extend it by declaring a module path. Catalog discovery stays dynamic
// within this exact upstream/organization and repository-name prefix.
type NamespaceConfig struct {
	ModulePrefix     string
	RepositoryPrefix string
	Upstream         string
	Organization     string
}

var ownerSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var repoPrefix = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (c NamespaceConfig) Validate() error {
	if err := ValidateModulePrefix(c.ModulePrefix); err != nil {
		return err
	}
	if !ownerSegment.MatchString(c.Upstream) || !ownerSegment.MatchString(c.Organization) {
		return errors.New("module proxy requires an exact upstream and organization")
	}
	if c.RepositoryPrefix != "" && !repoPrefix.MatchString(c.RepositoryPrefix) {
		return errors.New("module proxy repository prefix must be a bounded repository-name prefix")
	}
	return nil
}

// ValidateModulePrefix is shared by the HTTP route and pipeline environment.
func ValidateModulePrefix(prefix string) error {
	if prefix == "" || path.Clean(prefix) != prefix || strings.ContainsAny(prefix, "!*?[]\\") || module.CheckPath(prefix) != nil {
		return errors.New("module proxy requires a canonical module namespace without glob characters")
	}
	return nil
}
