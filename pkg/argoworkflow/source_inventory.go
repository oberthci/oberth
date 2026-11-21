package argoworkflow

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
	"strings"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

type slot struct {
	node *node
	name string
}
type originEvent struct {
	slot       slot
	descriptor sourcecontract.Descriptor
}
type sourceInventory struct {
	graph          *graph
	all, delivered map[slot]map[sourcecontract.Descriptor]bool
	reachable      map[*node]bool
	usesSource     bool
}

func (g *graph) sources() *sourceInventory {
	inv := &sourceInventory{graph: g, usesSource: g.sourceInputs().UsesSource, reachable: map[*node]bool{}, all: map[slot]map[sourcecontract.Descriptor]bool{}, delivered: map[slot]map[sourcecontract.Descriptor]bool{}}
	calls := append([]*call{}, g.roots...)
	for i := 0; i < len(calls); i++ {
		if calls[i].inspectionOnly {
			continue
		}
		n := calls[i].target
		if n != nil && !inv.reachable[n] {
			inv.reachable[n] = true
			calls = append(calls, edges(n)...)
		}
	}
	inv.propagate(inv.all, false)
	inv.propagate(inv.delivered, true)
	return inv
}

type sourceFlow struct {
	states map[slot]map[sourcecontract.Descriptor]bool
	links  map[slot][]slot
	events []originEvent
}

func (f *sourceFlow) add(s slot, d sourcecontract.Descriptor) {
	if f.states[s] == nil {
		f.states[s] = map[sourcecontract.Descriptor]bool{}
	}
	if !f.states[s][d] {
		f.states[s][d] = true
		f.events = append(f.events, originEvent{s, d})
	}
}
func (inv *sourceInventory) propagate(states map[slot]map[sourcecontract.Descriptor]bool, reachable bool) {
	f := sourceFlow{states: states, links: map[slot][]slot{}}
	if !reachable {
		inv.declarations(&f)
	}
	for _, c := range inv.graph.roots {
		f.wire(nil, c, reachable)
	}
	for _, n := range inv.graph.nodes {
		if reachable && !inv.reachable[n] {
			continue
		}
		for _, c := range edges(n) {
			f.wire(n, c, reachable)
		}
	}
	for i := 0; i < len(f.events); i++ {
		event := f.events[i]
		for _, target := range f.links[event.slot] {
			f.add(target, event.descriptor)
		}
	}
}
func (inv *sourceInventory) declarations(f *sourceFlow) {
	for _, n := range inv.graph.nodes {
		for _, a := range n.template.Inputs.Artifacts {
			d, _ := descriptor(a)
			f.add(slot{n, a.Name}, d)
		}
	}
}
func (f *sourceFlow) wire(parent *node, c *call, reachable bool) {
	if c.target == nil || reachable && c.inspectionOnly {
		return
	}
	for _, a := range c.target.template.Inputs.Artifacts {
		target := slot{c.target, a.Name}
		arg := c.arguments.Artifacts.GetArtifactByName(a.Name)
		if arg == nil {
			if reachable {
				d, _ := descriptor(a)
				f.add(target, d)
			}
			continue
		}
		f.forward(parent, target, arg.From)
	}
}
func (f *sourceFlow) forward(parent *node, target slot, from string) {
	name := inputReference(from)
	if name != "" && parent != nil && parent.template.Inputs.Artifacts.GetArtifactByName(name) != nil {
		source := slot{parent, name}
		f.links[source] = append(f.links[source], target)
	} else {
		f.add(target, sourcecontract.Descriptor{})
	}
}
func inputReference(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{{") || !strings.HasSuffix(value, "}}") {
		return ""
	}
	value = strings.TrimSpace(value[2 : len(value)-2])
	if !strings.HasPrefix(value, "inputs.artifacts.") || strings.ContainsAny(value, "{}[] \t\r\n") {
		return ""
	}
	return strings.TrimPrefix(value, "inputs.artifacts.")
}
func (inv *sourceInventory) takesSource(n *node) bool {
	for _, a := range n.template.Inputs.Artifacts {
		for d := range inv.all[slot{n, a.Name}] {
			if d.Kind.Valid() {
				return true
			}
		}
	}
	return false
}
func (inv *sourceInventory) validate() {
	pins := map[string]sourcecontract.Descriptor{}
	for _, n := range inv.graph.nodes {
		for _, a := range n.template.Inputs.Artifacts {
			inv.input(n, a, pins)
			if UsesCredentials(n.template) {
				inv.grantedSource(n, a)
			}
		}
		if inv.reachable[n] {
			inv.workingDirectory(n)
		}
	}
	if len(pins) > admissioncontract.MaxPinnedDestinations {
		inv.graph.refuse("spec.sources", "pin_count")
	}
}
func (inv *sourceInventory) input(n *node, a wfv1.Artifact, pins map[string]sourcecontract.Descriptor) {
	for d := range inv.all[slot{n, a.Name}] {
		if !d.Kind.Valid() {
			continue
		}
		inv.usesSource = true
		if !sourceDestination(a, d, leaf(n.template)) {
			inv.graph.refuse(sourcePath(n, a), "source")
		}
		if d.Kind == sourcecontract.Pinned && a.Path != "" {
			if prior, ok := pins[a.Path]; ok && prior != d {
				inv.graph.refuse(sourcePath(n, a), "source")
			}
			pins[a.Path] = d
		}
		inv.sourceMounts(n, a)
	}
}
func (inv *sourceInventory) grantedSource(n *node, a wfv1.Artifact) {
	states := inv.all[slot{n, a.Name}]
	if len(states) == 0 {
		inv.graph.refuse(sourcePath(n, a), "source")
		return
	}
	for d := range states {
		if !d.Kind.Valid() || a.Plugin == nil {
			inv.graph.refuse(sourcePath(n, a), "source")
		}
	}
}
func (inv *sourceInventory) sourceMounts(n *node, a wfv1.Artifact) {
	if a.Path == "" {
		return
	}
	for _, c := range containers(n.template) {
		inv.sourceContainerMounts(n, a, c.container.VolumeMounts)
	}
	if set := n.template.ContainerSet; set != nil {
		inv.sourceContainerMounts(n, a, set.VolumeMounts)
	}
}
func (inv *sourceInventory) sourceContainerMounts(n *node, a wfv1.Artifact, mounts []corev1.VolumeMount) {
	for _, m := range mounts {
		if overlaps(a.Path, m.MountPath) {
			inv.graph.refuse(sourcePath(n, a), "path")
		}
	}
}
func (inv *sourceInventory) workingDirectory(n *node) {
	for _, c := range containers(n.template) {
		if !c.main || n.template.ContainerSet != nil {
			continue
		}
		dir := c.container.WorkingDir
		if dir != "/work/src" && !strings.HasPrefix(dir, "/work/src/") {
			continue
		}
		covered := false
		for _, m := range c.container.VolumeMounts {
			if dir == m.MountPath || strings.HasPrefix(dir, strings.TrimSuffix(m.MountPath, "/")+"/") {
				covered = true
			}
		}
		if !covered && !inv.hasMain(n) {
			inv.graph.refuse(n.path+c.path+".workingDir", "source_usage")
		}
	}
}
func (inv *sourceInventory) hasMain(n *node) bool {
	for _, a := range n.template.Inputs.Artifacts {
		if a.Path != "/work/src" {
			continue
		}
		states := inv.delivered[slot{n, a.Name}]
		if len(states) == 0 {
			continue
		}
		main := true
		for d := range states {
			main = main && d.Kind == sourcecontract.Main
		}
		if main {
			return true
		}
	}
	return false
}

func (inv *sourceInventory) boundInputs() []SourceInput {
	result := []SourceInput{}
	for _, n := range inv.graph.nodes {
		if !leaf(n.template) {
			continue
		}
		for _, a := range n.template.Inputs.Artifacts {
			for d := range inv.all[slot{n, a.Name}] {
				if d.Kind.Valid() {
					result = append(result, SourceInput{Template: n.key, Artifact: a.Name, Path: a.Path, Descriptor: d, Shared: n.scope != ""})
				}
			}
		}
	}
	return result
}
