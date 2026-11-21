package sourcecontract

import (
	"reflect"
	"testing"
)

func TestProvenanceClonesPreserveValues(t *testing.T) {
	receipt := Receipt{Version: 1, Revision: Revision{Repository: "upstream/org/repo", Commit: "commit"}, ObjectSHA: "object", File: ".oberth/build.yaml", Blob: "blob", DocumentDigest: "digest"}
	binding := Binding{Template: "build", Artifact: "source", Path: "/work/src", Descriptor: Descriptor{Version: 1, Kind: "main"}, RepositoryID: 1, Revision: receipt.Revision, AuthorizationDigest: "authorization"}
	if receipt.Clone() != receipt {
		t.Fatal("receipt clone changed provenance")
	}
	if !reflect.DeepEqual(binding.Clone(), binding) {
		t.Fatal("binding clone changed evidence")
	}
	clone := binding.Clone()
	clone.Descriptor.Kind = "pin"
	if binding.Descriptor.Kind != "main" {
		t.Fatal("clone mutation changed original")
	}
}
