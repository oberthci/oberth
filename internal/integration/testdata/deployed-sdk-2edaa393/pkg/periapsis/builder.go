package periapsis

// PipelineBuilder provides the fluent .oberth/periapsis.go API.
type PipelineBuilder struct {
	burns []Burn
}

// New creates an empty pipeline builder.
func New() *PipelineBuilder {
	return &PipelineBuilder{}
}

// Retrograde appends a verification burn.
func (b *PipelineBuilder) Retrograde(name string, steps ...Step) *PipelineBuilder {
	return b.add(name, Retrograde, steps)
}

// Prograde appends a build burn.
func (b *PipelineBuilder) Prograde(name string, steps ...Step) *PipelineBuilder {
	return b.add(name, Prograde, steps)
}

// Escaped appends a release-only burn.
func (b *PipelineBuilder) Escaped(name string, steps ...Step) *PipelineBuilder {
	return b.add(name, Escaped, steps)
}

func (b *PipelineBuilder) add(name string, burnType BurnType, steps []Step) *PipelineBuilder {
	burn := Burn{Name: name, Type: burnType, Steps: make([]Step, len(steps))}
	for index := range steps {
		burn.Steps[index] = steps[index].clone()
	}
	b.burns = append(b.burns, burn)
	return b
}

// DependsOn replaces the dependencies of the most recently added burn.
func (b *PipelineBuilder) DependsOn(dependencies ...string) *PipelineBuilder {
	if len(b.burns) > 0 {
		b.burns[len(b.burns)-1].DependsOn = append([]string(nil), dependencies...)
	}
	return b
}

// Build returns a deep copy of the current pipeline.
func (b *PipelineBuilder) Build() Pipeline {
	return Pipeline{Burns: b.burns}.Clone()
}
