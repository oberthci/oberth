package staticgit

import _ "embed"

//go:embed build.env
var buildInputs string

//go:embed qualified-artifacts.json
var qualifiedArtifacts string
