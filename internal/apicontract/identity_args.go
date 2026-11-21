package apicontract

type AccessAllowArgs struct {
	Audience string `json:"audience,omitempty" desc:"Select the projected identity token audience approved for this workflow step." sensitive:"false" position:"0"`
	Claim    string `json:"claim,omitempty" desc:"Select the literal persistent-volume claim approved for this repository and tier." sensitive:"false" position:"0"`
	Kind     string `json:"kind,omitempty" desc:"Choose the resource category the operation reads or changes." sensitive:"false" position:"0"`
	Repo     string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"1"`
	Secret   string `json:"secret,omitempty" desc:"Select the exact secret path approved for the repository workflow and step." sensitive:"false" position:"0"`
	Step     string `json:"step,omitempty" desc:"Select the authored step or its unambiguous native identity; list the run to discover candidates." sensitive:"false" position:"0"`
	Tier     string `json:"tier,omitempty" desc:"Select the CI or release trust tier for this grant." sensitive:"false" position:"0"`
	Workflow string `json:"workflow,omitempty" desc:"Name the exact repository-relative workflow file covered by the grant." sensitive:"false" position:"0"`
	Reason   string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type AccessDismissArgs struct {
	Repo     string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"0"`
	Secret   string `json:"secret,omitempty" desc:"Select the exact secret path approved for the repository workflow and step." required:"true" sensitive:"false" position:"0"`
	Step     string `json:"step,omitempty" desc:"Select the authored step or its unambiguous native identity; list the run to discover candidates." required:"true" sensitive:"false" position:"0"`
	Workflow string `json:"workflow,omitempty" desc:"Name the exact repository-relative workflow file covered by the grant." required:"true" sensitive:"false" position:"0"`
	Reason   string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type AccessListArgs struct {
	Claim   string `json:"claim,omitempty" desc:"Select the literal persistent-volume claim approved for this repository and tier." sensitive:"false" position:"0"`
	Kind    string `json:"kind,omitempty" desc:"Choose the resource category the operation reads or changes." sensitive:"false" position:"0"`
	Pending bool   `json:"pending,omitempty" desc:"Include observed declarations that still need an operator grant." sensitive:"false" position:"0"`
	Repo    string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	Revoked bool   `json:"revoked,omitempty" desc:"Include revoked grants in the authorized result." sensitive:"false" position:"0"`
	Tier    string `json:"tier,omitempty" desc:"Select the CI or release trust tier for this grant." sensitive:"false" position:"0"`
}

type AccessRevokeArgs struct {
	Audience string `json:"audience,omitempty" desc:"Select the projected identity token audience approved for this workflow step." sensitive:"false" position:"0"`
	Claim    string `json:"claim,omitempty" desc:"Select the literal persistent-volume claim approved for this repository and tier." sensitive:"false" position:"0"`
	Kind     string `json:"kind,omitempty" desc:"Choose the resource category the operation reads or changes." sensitive:"false" position:"0"`
	Repo     string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"1"`
	Secret   string `json:"secret,omitempty" desc:"Select the exact secret path approved for the repository workflow and step." sensitive:"false" position:"0"`
	Step     string `json:"step,omitempty" desc:"Select the authored step or its unambiguous native identity; list the run to discover candidates." sensitive:"false" position:"0"`
	Tier     string `json:"tier,omitempty" desc:"Select the CI or release trust tier for this grant." sensitive:"false" position:"0"`
	Workflow string `json:"workflow,omitempty" desc:"Name the exact repository-relative workflow file covered by the grant." sensitive:"false" position:"0"`
	Reason   string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type AdminAccessListArgs struct {
}

type AdminAuditRecordArgs struct {
	Action  string            `json:"action,omitempty" desc:"Select the lifecycle operation or repair action to observe or record." required:"true" sensitive:"false" position:"1"`
	Details map[string]string `json:"details,omitempty" desc:"Supply only public scalar lifecycle receipt facts; credentials and raw dependency errors do not belong here." sensitive:"false" position:"0"`
	Reason  string            `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type AdminIdleArgs struct {
}

type AdminPublicIdentityArgs struct {
	Name string `json:"name,omitempty" desc:"Select the registered upstream or named public identity resource for this operation." required:"true" sensitive:"false" position:"1"`
}

type AdminReadinessArgs struct {
}

type AdminVersionArgs struct {
}

type ClientRenewArgs struct {
	CSR    string `json:"csr,omitempty" desc:"Supply a public certificate request generated with the matching local private key." required:"true" sensitive:"false" position:"1"`
	TTL    string `json:"ttl,omitempty" desc:"Select the requested credential lifetime within the issuer policy." sensitive:"false" position:"0"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type RepoAddArgs struct {
	DefaultBranch string `json:"default_branch,omitempty" desc:"Name the default branch for the repository mapping; omission uses the configured default." sensitive:"false" position:"0"`
	Repo          string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"0"`
	Upstream      string `json:"upstream,omitempty" desc:"Choose the registered upstream that owns the repository or issue." required:"true" sensitive:"false" position:"0"`
	Reason        string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type RepoArchiveArgs struct {
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type RepoListArgs struct {
	NamesOnly bool   `json:"names_only,omitempty" desc:"Read repository names and archive state without Git facts or latest runs." sensitive:"false" position:"0"`
	Repo      string `json:"repo,omitempty" desc:"Read facts for one visible repository." format:"repo" sensitive:"false" position:"0"`
	Archived  string `json:"archived,omitempty" desc:"Choose whether archived repository mappings are excluded, included or selected alone." sensitive:"false" position:"0"`
}

type RepoRemoveArgs struct {
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type RepoUnarchiveArgs struct {
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type RepoVerifyArgs struct {
	Repo string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"1"`
}

type UplinkAddArgs struct {
	Admin        bool     `json:"admin,omitempty" desc:"Request administrator authority only when the authenticated operator may grant it." sensitive:"false" position:"0"`
	BearerDigest string   `json:"bearer_digest,omitempty" desc:"Activate the SHA256 digest of a bearer already generated and retained by the caller." sensitive:"false" position:"0"`
	CSR          string   `json:"csr,omitempty" desc:"Supply a public certificate request generated with the matching local private key." sensitive:"false" position:"0"`
	Identity     string   `json:"identity,omitempty" desc:"Name the approved uplink identity; this never selects a local filesystem path." required:"true" sensitive:"false" position:"0"`
	Scope        []string `json:"scope,omitempty" desc:"Set the approved upstream names; an explicit empty array clears repository restrictions." maxItems:"128" sensitive:"false" position:"0"`
	SshPublicKey string   `json:"ssh_public_key,omitempty" desc:"Supply a public SSH key for Git pushes; omit it for a browser-only bearer." sensitive:"false" position:"0"`
	TTL          string   `json:"ttl,omitempty" desc:"Select the requested credential lifetime within the issuer policy." sensitive:"false" position:"0"`
	Reason       string   `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type UplinkCertArgs struct {
	CSR         string `json:"csr,omitempty" desc:"Supply a public certificate request generated with the matching local private key." required:"true" sensitive:"false" position:"0"`
	Fingerprint string `json:"fingerprint,omitempty" desc:"Select the exact registered SSH key fingerprint when an identity has several keys." sensitive:"false" position:"0"`
	Identity    string `json:"identity,omitempty" desc:"Name the approved uplink identity; this never selects a local filesystem path." required:"true" sensitive:"false" position:"0"`
	TTL         string `json:"ttl,omitempty" desc:"Select the requested credential lifetime within the issuer policy." sensitive:"false" position:"0"`
	Reason      string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type UplinkInviteArgs struct {
	Expires      string   `json:"expires,omitempty" desc:"Select the requested expiry within the configured policy lifetime." sensitive:"false" position:"0"`
	Identity     string   `json:"identity,omitempty" desc:"Name the approved uplink identity; this never selects a local filesystem path." required:"true" sensitive:"false" position:"0"`
	Scope        []string `json:"scope,omitempty" desc:"Set the approved upstream names; an explicit empty array clears repository restrictions." sensitive:"false" position:"0"`
	SecretDigest string   `json:"secret_digest,omitempty" desc:"Supply the digest of the random invitation secret generated and retained locally." required:"true" sensitive:"false" position:"0"`
	ServerURL    string   `json:"server_url,omitempty" desc:"Bind the invitation to the public HTTPS MCP endpoint the recipient will use." required:"true" sensitive:"false" position:"0"`
	Reason       string   `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type UplinkListArgs struct {
}

type UplinkRevokeArgs struct {
	Fingerprint string `json:"fingerprint,omitempty" desc:"Select the exact registered SSH key fingerprint when an identity has several keys." sensitive:"false" position:"0"`
	Identity    string `json:"identity,omitempty" desc:"Name the approved uplink identity; this never selects a local filesystem path." required:"true" sensitive:"false" position:"1"`
	Reason      string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type UplinkRotateArgs struct {
	Fingerprint            string `json:"fingerprint,omitempty" desc:"Select the exact registered SSH key fingerprint when an identity has several keys." required:"true" sensitive:"false" position:"0"`
	Identity               string `json:"identity,omitempty" desc:"Name the approved uplink identity; this never selects a local filesystem path." required:"true" sensitive:"false" position:"0"`
	ReplacementFingerprint string `json:"replacement_fingerprint,omitempty" desc:"Select an already registered replacement key with the same approved identity and scope." required:"true" sensitive:"false" position:"0"`
	Reason                 string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type UplinkScopeArgs struct {
	Fingerprint string     `json:"fingerprint,omitempty" desc:"Select the exact registered SSH key fingerprint when an identity has several keys." sensitive:"false" position:"0"`
	Identity    string     `json:"identity,omitempty" desc:"Name the approved uplink identity; this never selects a local filesystem path." required:"true" sensitive:"false" position:"0"`
	Scope       *[]*string `json:"scope,omitempty" desc:"Set the approved upstream names; an explicit empty array clears repository restrictions." required:"true" maxItems:"128" sensitive:"false" position:"0"`
	Reason      string     `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type UpstreamAddArgs struct {
	BaseURL                 string `json:"base_url,omitempty" desc:"Supply the upstream repository namespace URL; local paths and command strings are not accepted." required:"true" sensitive:"false" position:"0"`
	CheckRepo               string `json:"check_repo,omitempty" desc:"Probe this repository with the configured upstream identity." sensitive:"false" position:"0"`
	DedicatedKey            bool   `json:"dedicated_key,omitempty" desc:"Use a dedicated upstream identity instead of the shared server identity." sensitive:"false" position:"0"`
	ExpectedHostFingerprint string `json:"expected_host_fingerprint,omitempty" desc:"Confirm the expected remote host key before accepting the upstream binding." sensitive:"false" position:"0"`
	Name                    string `json:"name,omitempty" desc:"Select the registered upstream or named public identity resource for this operation." required:"true" sensitive:"false" position:"0"`
	NoWait                  bool   `json:"no_wait,omitempty" desc:"Return after registration instead of waiting for an upstream probe." sensitive:"false" position:"0"`
	Repin                   bool   `json:"repin,omitempty" desc:"Explicitly replace the stored host-key pin after verifying the new fingerprint." sensitive:"false" position:"0"`
	Reason                  string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type UpstreamListArgs struct {
	Check bool `json:"check,omitempty" desc:"Probe registered upstream access as part of this read." sensitive:"false" position:"0"`
}

type UpstreamProvideKeyArgs struct {
	DedicatedKey bool   `json:"dedicated_key,omitempty" desc:"Use a dedicated upstream identity instead of the shared server identity." sensitive:"false" position:"0"`
	Name         string `json:"name,omitempty" desc:"Select the registered upstream or named public identity resource for this operation." required:"true" sensitive:"false" position:"0"`
	PrivateKey   string `json:"private_key,omitempty" desc:"Import private key text through this authenticated body only; never use a key filename." required:"true" sensitive:"true" position:"0"`
	Reason       string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type UpstreamRemoveArgs struct {
	Name   string `json:"name,omitempty" desc:"Select the registered upstream or named public identity resource for this operation." required:"true" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type WorkflowCapabilityGetArgs struct {
	Repo string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"1"`
}

type WorkflowCapabilityGrantArgs struct {
	Capability WorkflowCapabilityGrantArgsCapability `json:"capability,omitempty" desc:"Describe the operator-owned namespace, service account and allowed native API operations." required:"true" sensitive:"false" position:"0"`
	Repo       string                                `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"0"`
	Reason     string                                `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type WorkflowCapabilityRevokeArgs struct {
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}
