package apicontract

type PromoteArgs struct {
	Branch string `json:"branch,omitempty" desc:"Optionally name the fresh default target branch; omission selects the repository default." sensitive:"false" position:"0"`
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	SHA    string `json:"sha,omitempty" desc:"Select the exact tested commit to promote; use its full object ID." required:"true" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type PromoteRetryArgs struct {
	ID     string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type PromoteStatusArgs struct {
	ID       string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
	TimeoutS int    `json:"timeout_s,omitempty" desc:"Wait at most this many seconds; zero or omission selects the advertised default." min:"0" max:"600" default:"600" timeout:"true" sensitive:"false" position:"0"`
}

type PromotionListArgs struct {
	Before    string `json:"before,omitempty" desc:"Continue an older page using the record-ID cursor from the preceding reply." sensitive:"false" position:"0"`
	Limit     int    `json:"limit,omitempty" desc:"Bound the number of records returned; continue with the cursor in the reply." min:"1" max:"200" default:"50" sensitive:"false" position:"0"`
	Repo      string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	SourceSHA string `json:"source_sha,omitempty" desc:"Filter promotion records by an exact tested source commit." sensitive:"false" position:"0"`
	Status    string `json:"status,omitempty" desc:"Filter by the committed public state; run success and publication delivery are separate facts." sensitive:"false" position:"0"`
}

type PublicationAbandonArgs struct {
	ID     string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"0"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." required:"true" sensitive:"false" position:"0"`
}

type PublishRetryArgs struct {
	ID     string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type ReceiveOutboxArgs struct {
	Action string `json:"action,omitempty" desc:"Select the lifecycle operation or repair action to observe or record." required:"true" sensitive:"false" position:"1"`
	ID     string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." sensitive:"false" position:"0"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
}

type RunCancelArgs struct {
	Force  bool   `json:"force,omitempty" desc:"Confirm the explicitly authorized override; it grants no additional authority." sensitive:"false" position:"0"`
	ID     string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type RunHoldAbandonArgs struct {
	ID     string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
	Step   string `json:"step,omitempty" desc:"Select the authored step or its unambiguous native identity; list the run to discover candidates." sensitive:"false" position:"0"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type RunHoldExecArgs struct {
	Argv     []string `json:"argv,omitempty" desc:"Pass an argument vector directly to the held step; no shell command string is constructed." required:"true" maxItems:"256" minItems:"1" sensitive:"false" position:"0"`
	ID       string   `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"0"`
	Step     string   `json:"step,omitempty" desc:"Select the authored step or its unambiguous native identity; list the run to discover candidates." sensitive:"false" position:"0"`
	TimeoutS int      `json:"timeout_s,omitempty" desc:"Wait at most this many seconds; zero or omission selects the advertised default." min:"0" max:"900" default:"60" timeout:"true" sensitive:"false" position:"0"`
	Reason   string   `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type RunHoldListArgs struct {
	ID string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
}

type RunPurgeArgs struct {
	ID     string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"0"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." required:"true" sensitive:"false" position:"0"`
	What   string `json:"what,omitempty" desc:"Select the retained evidence category the authorized purge removes." required:"true" sensitive:"false" position:"0"`
}

type RunRerunArgs struct {
	Assist bool   `json:"assist,omitempty" desc:"Request assistance only where repository policy and the selected run permit it." sensitive:"false" position:"0"`
	ID     string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type RunWaitArgs struct {
	ID       string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
	TimeoutS int    `json:"timeout_s,omitempty" desc:"Wait at most this many seconds; zero or omission selects the advertised default." min:"0" max:"600" default:"600" timeout:"true" sensitive:"false" position:"0"`
	Until    string `json:"until,omitempty" desc:"Choose finished, held or any observable change as the condition that ends this wait." sensitive:"false" position:"0"`
}

type SyncArgs struct {
	Branch string `json:"branch,omitempty" desc:"Name the target branch without the refs/heads/ prefix." sensitive:"false" position:"0"`
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	SHA    string `json:"sha,omitempty" desc:"Select the exact source object or an unambiguous visible hexadecimal prefix." required:"true" sensitive:"false" position:"1"`
	Reason string `json:"reason,omitempty" desc:"Explain why this action is requested; the service records it with the audit outcome." sensitive:"false" position:"0"`
}

type PublicationListArgs struct {
	Repo   string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	Status string `json:"status,omitempty" desc:"Filter by the committed public state; run success and publication delivery are separate facts." sensitive:"false" position:"0"`
	Before string `json:"before,omitempty" desc:"Continue an older page using the record-ID cursor from the preceding reply." sensitive:"false" position:"0"`
	Limit  int    `json:"limit,omitempty" desc:"Bound the number of records returned; continue with the cursor in the reply." min:"0" max:"200" default:"50" sensitive:"false" position:"0"`
}

type RunResumeArgs struct {
	ID     string `json:"id,omitempty" required:"true" desc:"Select the failed run to resume." sensitive:"false" position:"1"`
	Commit string `json:"commit,omitempty" required:"true" desc:"Select the full pushed fix commit at the branch head." sensitive:"false" position:"2"`
}

type ResumeResult struct {
	Run                 string `json:"run"`
	Mode                string `json:"mode"`
	Detail              string `json:"detail"`
	DeadlineRemainingMS *int64 `json:"deadline_remaining_ms,omitempty"`
	Advice              string `json:"advice,omitempty"`
}
