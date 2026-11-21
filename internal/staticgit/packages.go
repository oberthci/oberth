package staticgit

import (
	"fmt"
	"strings"
)

func verifyPackages(pins, installed string) error {
	versions := packageVersions(installed)
	wanted := strings.Fields(pins)
	for _, pin := range wanted {
		name, version, ok := strings.Cut(pin, "=")
		if !ok || versions[name] != version {
			return fmt.Errorf("builder package %s differs from its pin; install the exact reviewed package version", name)
		}
	}
	return nil
}

func packageVersions(installed string) map[string]string {
	versions := make(map[string]string)
	for _, block := range strings.Split(installed, "\n\n") {
		name, version := "", ""
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(line, ":")
			switch key {
			case "P":
				name = value
			case "V":
				version = value
			}
		}
		if name != "" {
			versions[name] = version
		}
	}
	return versions
}
