package apicontract

type IssueImportArgs struct {
	Upstream string `json:"upstream,omitempty" desc:"Map every source upstream and qualified repository to this registered local upstream; omission keeps source names." sensitive:"false" position:"0"`
	JSONL    string `json:"jsonl" desc:"Supply a complete JSON Lines file: a count header followed by that many manual issues." required:"true" sensitive:"true" position:"0"`
	DryRun   bool   `json:"dry_run,omitempty" desc:"Validate the complete file and conflicts without writing." sensitive:"false" position:"0"`
}
