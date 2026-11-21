package apicontract

type LogsArgs struct {
	ByteOffset *int64  `json:"byte_offset,omitempty" desc:"Continue the retained byte window from next_byte_offset; keep the same filter and direction." min:"0" sensitive:"false" position:"0"`
	Container  string  `json:"container,omitempty" desc:"Select the native container name from the run container inventory." sensitive:"false" position:"0"`
	Containers bool    `json:"containers,omitempty" desc:"Return the authorized native container inventory instead of log bytes." sensitive:"false" position:"0"`
	Context    *int    `json:"context,omitempty" desc:"Include this many neighboring lines around each matching log line." min:"0" max:"50" sensitive:"false" position:"0"`
	Limit      *int    `min:"0" json:"limit,omitempty" desc:"Bound the number of records returned; continue with the cursor in the reply." sensitive:"false" position:"0"`
	NodeID     string  `json:"node_id,omitempty" desc:"Select the native node ID from the run container inventory." sensitive:"false" position:"0"`
	Offset     *int    `json:"offset,omitempty" desc:"Skip lines or matches inside the scanned window; reset to zero when continuing a byte cursor." min:"0" sensitive:"false" position:"0"`
	Pattern    *string `json:"pattern,omitempty" desc:"Filter log lines using this regular expression before returning a bounded window." maxBytes:"512" sensitive:"false" position:"0"`
	Raw        bool    `json:"raw,omitempty" desc:"Read retained bytes without interpreting display text as protocol markers." sensitive:"false" position:"0"`
	Repo       string  `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	SHA        string  `json:"sha,omitempty" desc:"Select the exact source object or an unambiguous visible hexadecimal prefix." required:"true" sensitive:"false" position:"1"`
	Step       string  `json:"step,omitempty" desc:"Select the authored step or its unambiguous native identity; list the run to discover candidates." sensitive:"false" position:"0"`
	Tail       *bool   `json:"tail,omitempty" desc:"Read backward from the retained end instead of forward from the beginning." sensitive:"false" position:"0"`
}

type RunCheckArgs struct {
	ReleaseTag string                  `json:"release_tag,omitempty" desc:"Preflight this proposed release tag at ref, including live release grants, without creating a tag or run." sensitive:"false" position:"0"`
	Repo       string                  `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." required:"true" format:"repo" sensitive:"false" position:"1"`
	Ref        string                  `json:"ref,omitempty" desc:"Select a branch, tag or exact revision in the authorized repository." sensitive:"false" position:"0"`
	Files      []RunCheckArgsFilesItem `json:"files,omitempty" desc:"Overlay regular authored files for inspection only; nothing is admitted or persisted." maxItems:"32" sensitive:"false" position:"0"`
}

type RunGetArgs struct {
	ID         string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
	StepLimit  int    `json:"step_limit,omitempty" desc:"Bound the requested logical-step page; step counts still describe the complete admitted plan." min:"0" max:"100" default:"100" sensitive:"false" position:"0"`
	StepOffset int    `json:"step_offset,omitempty" desc:"Continue at next_step_offset from the preceding logical-step page." min:"0" sensitive:"false" position:"0"`
	View       string `json:"view,omitempty" desc:"Choose summary, full or dashboard detail; summary is the default and step pages remain bounded." default:"summary" sensitive:"false" position:"0"`
}

type RunListArgs struct {
	Before  string `json:"before,omitempty" desc:"Continue an older page using the record-ID cursor from the preceding reply." sensitive:"false" position:"0"`
	Held    bool   `json:"held,omitempty" desc:"Select runs with an active assistance hold." sensitive:"false" position:"0"`
	Latest  bool   `json:"latest,omitempty" desc:"Return the latest visible run for each matching repository and reference." sensitive:"false" position:"0"`
	Limit   int    `json:"limit,omitempty" desc:"Bound the number of records returned; continue with the cursor in the reply." min:"0" max:"100" default:"50" sensitive:"false" position:"0"`
	Own     bool   `json:"own,omitempty" desc:"Include only observations attributed to the authenticated identity." sensitive:"false" position:"0"`
	Ref     string `json:"ref,omitempty" desc:"Select a branch, tag or exact revision in the authorized repository." sensitive:"false" position:"0"`
	RefGlob string `json:"ref_glob,omitempty" desc:"Filter run references with a shell-style pattern before pagination." sensitive:"false" position:"0"`
	Repo    string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	Status  string `json:"status,omitempty" desc:"Filter by the committed public state; run success and publication delivery are separate facts." sensitive:"false" position:"0"`
	View    string `json:"view,omitempty" desc:"Choose all visible runs or the older history view while preserving the returned cursor." default:"all" sensitive:"false" position:"0"`
}

type RunLogsArgs struct {
	ByteOffset *int64  `json:"byte_offset,omitempty" desc:"Continue the retained byte window from next_byte_offset; keep the same filter and direction." min:"0" sensitive:"false" position:"0"`
	Container  string  `json:"container,omitempty" desc:"Select the native container name from the run container inventory." sensitive:"false" position:"0"`
	Containers bool    `json:"containers,omitempty" desc:"Return the authorized native container inventory instead of log bytes." sensitive:"false" position:"0"`
	Context    *int    `json:"context,omitempty" desc:"Include this many neighboring lines around each matching log line." min:"0" max:"50" sensitive:"false" position:"0"`
	ID         string  `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." required:"true" sensitive:"false" position:"1"`
	Limit      *int    `min:"0" json:"limit,omitempty" desc:"Bound the number of records returned; continue with the cursor in the reply." sensitive:"false" position:"0"`
	NodeID     string  `json:"node_id,omitempty" desc:"Select the native node ID from the run container inventory." sensitive:"false" position:"0"`
	Offset     *int    `json:"offset,omitempty" desc:"Skip lines or matches inside the scanned window; reset to zero when continuing a byte cursor." min:"0" sensitive:"false" position:"0"`
	Pattern    *string `json:"pattern,omitempty" desc:"Filter log lines using this regular expression before returning a bounded window." maxBytes:"512" sensitive:"false" position:"0"`
	Raw        bool    `json:"raw,omitempty" desc:"Read retained bytes without interpreting display text as protocol markers." sensitive:"false" position:"0"`
	Step       string  `json:"step,omitempty" desc:"Select the authored step or its unambiguous native identity; list the run to discover candidates." sensitive:"false" position:"0"`
	Tail       *bool   `json:"tail,omitempty" desc:"Read backward from the retained end instead of forward from the beginning." sensitive:"false" position:"0"`
}

type StatusArgs struct {
	Ref  string `json:"ref,omitempty" desc:"Select a branch, tag or exact revision in the authorized repository." required:"true" sensitive:"false" position:"1"`
	Repo string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
}

type WaitArgs struct {
	Ref      string `json:"ref,omitempty" desc:"Select the authorized source reference to observe without submitting work." sensitive:"false" position:"0"`
	ID       string `json:"id,omitempty" desc:"Select the exact record ID returned by a list or action; run prefixes need at least eight hex characters." sensitive:"false" position:"0"`
	Repo     string `json:"repo,omitempty" desc:"Select a visible repository by bare name or canonical upstream/organization/name." format:"repo" sensitive:"false" position:"0"`
	SHA      string `json:"sha,omitempty" desc:"Select the exact source object or an unambiguous visible hexadecimal prefix." sensitive:"false" position:"0"`
	TimeoutS int    `json:"timeout_s,omitempty" desc:"Wait at most this many seconds; zero or omission selects the advertised default." min:"0" max:"600" default:"600" timeout:"true" sensitive:"false" position:"0"`
	Trigger  string `json:"trigger,omitempty" desc:"Filter runs by trigger: branch, promotion, schedule or tag; omission includes all triggers." sensitive:"false" position:"0"`
}
