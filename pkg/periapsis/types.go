// Package periapsis defines the typed pipeline contract interpreted by the
// Oberth job runner.
package periapsis

import (
	"maps"
	"slices"
	"time"
)

// BurnType classifies a pipeline phase.
type BurnType int

const (
	// Retrograde burns verify source without release credentials.
	Retrograde BurnType = iota + 1
	// Prograde burns build source without release credentials.
	Prograde
	// Release burns execute only for an admitted release-tag trigger.
	Release
)

// Duration constants let an interpreted pipeline declare a timeout without
// importing the time package.
const (
	Second time.Duration = time.Second
	Minute time.Duration = time.Minute
	Hour   time.Duration = time.Hour
)

func (b BurnType) String() string {
	switch b {
	case Retrograde:
		return "retrograde"
	case Prograde:
		return "prograde"
	case Release:
		return "release"
	default:
		return "unknown"
	}
}

// Pipeline is the value returned by Pipeline(ctx) in .oberth/periapsis.go.
type Pipeline struct {
	Burns []Burn
}

// Clone returns a deep copy that can be selected or reordered independently.
func (p Pipeline) Clone() Pipeline {
	clone := Pipeline{Burns: make([]Burn, len(p.Burns))}
	for index := range p.Burns {
		clone.Burns[index] = p.Burns[index].clone()
	}
	return clone
}

// Burn is a named, ordered collection of subprocess steps.
type Burn struct {
	Name      string
	Type      BurnType
	DependsOn []string
	Steps     []Step
}

func (b Burn) clone() Burn {
	b.DependsOn = slices.Clone(b.DependsOn)
	steps := b.Steps
	b.Steps = make([]Step, len(steps))
	for index := range steps {
		b.Steps[index] = steps[index].clone()
	}
	return b
}

// Step describes one direct subprocess invocation.
type Step struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	Timeout time.Duration

	argInsertionPoint int
}

func (s Step) clone() Step {
	s.Args = slices.Clone(s.Args)
	s.Env = maps.Clone(s.Env)
	return s
}

func (s Step) withArgInsertionPoint(index int) Step {
	if index >= 0 && index <= len(s.Args) {
		s.argInsertionPoint = index + 1
	}
	return s
}

// WithEnv returns a copy with one environment override.
func (s Step) WithEnv(key, value string) Step {
	s = s.clone()
	if s.Env == nil {
		s.Env = make(map[string]string, 1)
	}
	s.Env[key] = value
	return s
}

// WithArgs returns a copy with arguments inserted before the SDK method's
// terminal package/path operand. Direct Step values append arguments.
func (s Step) WithArgs(args ...string) Step {
	s = s.clone()
	if s.argInsertionPoint > 0 {
		index := s.argInsertionPoint - 1
		if index <= len(s.Args) {
			s.Args = slices.Insert(s.Args, index, args...)
			s.argInsertionPoint += len(args)
			return s
		}
	}
	s.Args = append(s.Args, args...)
	return s
}

// WithTimeout returns a copy with a step-specific timeout.
func (s Step) WithTimeout(timeout time.Duration) Step {
	s = s.clone()
	s.Timeout = timeout
	return s
}
