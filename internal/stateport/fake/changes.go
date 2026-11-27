package fake

import (
	"reflect"

	"github.com/oberthci/oberth/internal/stateport"
)

func (f *Runs) Subscribe(scope stateport.ChangeScope) stateport.Subscription {
	return f.notifications.Subscribe(scope)
}

func (f *Runs) committedChanges(next runMemory) []stateport.Change {
	var changes []stateport.Change
	for id, r := range next.runs {
		if !reflect.DeepEqual(f.data.runs[id], r) || !reflect.DeepEqual(f.data.native[id], next.native[id]) || !reflect.DeepEqual(f.data.purges[id], next.purges[id]) {
			changes = append(changes, stateport.Change{Repository: r.Repo, Run: id})
		}
	}
	for _, e := range next.events {
		if e.ID > f.data.eventID {
			changes = append(changes, stateport.Change{Repository: e.Repo, Run: e.RunID, Upstream: stateport.EventScope(e)})
		}
	}
	for repo, through := range next.pruned {
		if through != f.data.pruned[repo] {
			changes = append(changes, stateport.Change{Repository: repo, Invalidate: true})
		}
	}
	changes = append(changes, f.controlChanges(next)...)
	if next.grantRevision != f.data.grantRevision {
		changes = append(changes, stateport.Change{Authority: true})
	}
	return changes
}

func (f *Runs) controlChanges(next runMemory) []stateport.Change {
	var changes []stateport.Change
	for _, r := range changedValues(f.data.repos, next.repos) {
		changes = append(changes, stateport.Change{Repository: r.Canonical, Invalidate: true})
	}
	for _, p := range changedValues(f.data.promotions, next.promotions) {
		changes = append(changes, stateport.Change{Repository: p.Repo})
	}
	for _, p := range changedValues(f.data.publications, next.publications) {
		changes = append(changes, stateport.Change{Repository: p.Repo})
	}
	for _, issue := range changedValues(f.data.issues, next.issues) {
		changes = append(changes, stateport.Change{Repository: issue.Repo, Upstream: issue.Upstream})
	}
	for _, a := range next.audits[len(f.data.audits):] {
		switch a.ResourceType {
		case "key", "installation", "upstream", "invitation":
			changes = append(changes, stateport.Change{Authority: true})
		}
	}
	return changes
}

// Include both sides of scope moves and removal, as the durable writer does.
func changedValues[K comparable, V any](previous, next map[K]V) []V {
	var values []V
	for id, old := range previous {
		if !reflect.DeepEqual(old, next[id]) {
			values = append(values, old)
		}
	}
	for id, current := range next {
		if !reflect.DeepEqual(previous[id], current) {
			values = append(values, current)
		}
	}
	return values
}
