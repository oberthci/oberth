package model

// IdleQueueStatus is an unscoped, read-only snapshot of nonterminal runs for
// the pod-private upgrade observer. Unknown triggers remain critical.
type IdleQueueStatus struct {
	Version   int `json:"version"`
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Branch    int `json:"branch"`
	Tag       int `json:"tag"`
	Promotion int `json:"promotion"`
	Plan      int `json:"plan"`
	Apply     int `json:"apply"`
	Other     int `json:"other"`
}

func (s IdleQueueStatus) Critical() int { return s.Tag + s.Promotion + s.Plan + s.Apply + s.Other }
func (s IdleQueueStatus) Valid() bool {
	for _, n := range []int{s.Queued, s.Running, s.Branch, s.Tag, s.Promotion, s.Plan, s.Apply, s.Other} {
		if n < 0 || n > 1_000_000_000 {
			return false
		}
	}
	return s.Version == 1 && s.Branch+s.Critical() == s.Queued+s.Running
}
