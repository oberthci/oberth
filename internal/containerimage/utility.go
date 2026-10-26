// Package containerimage holds shared operator-owned utility image pins.
package containerimage

// Utility supplies shell/file tools to maintenance and source-seed containers.
// The scratch server image must never be used to run those tools.
const Utility = "busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74"

// ServerCommand guarantees that an exec aimed at the scratch server cannot
// accidentally invoke a shell/file tool. Bare oberth is canonicalized for
// existing callers; all returned argv use the absolute executable path.
func ServerCommand(command []string) []string {
	if len(command) == 0 {
		panic("scratch server exec requires oberth argv")
	}
	result := append([]string(nil), command...)
	if result[0] == "oberth" {
		result[0] = "/usr/local/bin/oberth"
	}
	if result[0] != "/usr/local/bin/oberth" {
		panic("scratch server exec requires /usr/local/bin/oberth")
	}
	return result
}
