package apicontract

type Empty struct{}
type IDArgs struct {
	ID string `json:"id" required:"true" desc:"Select the exact authorized run ID." sensitive:"false" position:"1"`
}
type ContainerArgs struct {
	ID         string  `json:"id" required:"true" desc:"Select the authorized run ID." sensitive:"false" position:"0"`
	NodeID     string  `json:"node_id" required:"true" desc:"Select the native node from the container inventory." sensitive:"false" position:"0"`
	Container  string  `json:"container" required:"true" desc:"Select the native container from the inventory." sensitive:"false" position:"0"`
	Follow     bool    `json:"follow,omitempty" desc:"Continue observing bytes until the source ends." sensitive:"false" position:"0"`
	Pattern    *string `json:"pattern,omitempty" desc:"Filter retained lines by a regular expression." maxBytes:"512" sensitive:"false" position:"0"`
	Context    *int    `json:"context,omitempty" desc:"Include this many neighboring lines per matching line." min:"0" max:"50" sensitive:"false" position:"0"`
	Offset     *int    `json:"offset,omitempty" desc:"Skip this many matching or retained lines in the window." min:"0" sensitive:"false" position:"0"`
	ByteOffset *int64  `json:"byte_offset,omitempty" desc:"Continue from the preceding retained-byte cursor." min:"0" sensitive:"false" position:"0"`
	Limit      *int    `json:"limit,omitempty" desc:"Bound the number of matching or retained lines." min:"0" sensitive:"false" position:"0"`
	Tail       *bool   `json:"tail,omitempty" desc:"Read backward from the retained end when requested." sensitive:"false" position:"0"`
}
