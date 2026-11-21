package apicontract

type RunCheckArgsFilesItem struct {
	Path    string `json:"path,omitempty" desc:"Name the repository-relative authored file or exact approved resource path." required:"true" sensitive:"false" position:"0"`
	Content string `json:"content,omitempty" desc:"Supply the complete UTF-8 authored file contents for this inspection overlay." required:"true" sensitive:"false" position:"0"`
}

type WorkflowCapabilityGrantArgsCapabilityResourceKindsItem struct {
	Actions    []string `json:"actions,omitempty" desc:"List the native API operations permitted for this resource kind." required:"true" maxItems:"4" minItems:"1" sensitive:"false" position:"0"`
	ApiVersion string   `json:"api_version,omitempty" desc:"Name the exact native API version, including its group when required." required:"true" sensitive:"false" position:"0"`
	Kind       string   `json:"kind,omitempty" desc:"Choose the resource category the operation reads or changes." required:"true" sensitive:"false" position:"0"`
}

type WorkflowCapabilityGrantArgsCapability struct {
	Namespace      string                                                   `json:"namespace,omitempty" desc:"Name the configured execution namespace owned by the operator." required:"true" sensitive:"false" position:"0"`
	ResourceKinds  []WorkflowCapabilityGrantArgsCapabilityResourceKindsItem `json:"resource_kinds,omitempty" desc:"List the exact native API resource kinds and operations the capability permits." maxItems:"32" sensitive:"false" position:"0"`
	ServiceAccount string                                                   `json:"service_account,omitempty" desc:"Name the operator-provided service account used for this capability." required:"true" sensitive:"false" position:"0"`
}
