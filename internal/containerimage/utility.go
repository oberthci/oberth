// Package containerimage holds shared operator-owned utility image pins.
package containerimage

// Utility supplies shell/file tools to maintenance and source-seed containers.
// The scratch server image must never be used to run those tools.
const Utility = "busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74"
