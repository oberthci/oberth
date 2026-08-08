package job

import "errors"

// ErrRetainedLogLimit identifies the deliberate destination failure used to
// stop an otherwise unbounded run before it can exhaust Oberth's PVC.
var ErrRetainedLogLimit = errors.New("run log exceeds the retained byte limit")
