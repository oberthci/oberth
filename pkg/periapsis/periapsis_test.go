package periapsis

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const representativeSource = `//go:build ignore

package main

import "oberth"

func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	return oberth.New().
		Retrograde("lint", ctx.Go.Vet("./...").WithArgs("-tags=ci")).
		Retrograde("test", ctx.Go.TestRace("./...").WithTimeout(2 * oberth.Minute)).DependsOn("lint").
		Prograde("build", ctx.Go.Build("./cmd/app").WithEnv("GOOS", "linux")).DependsOn("test").
		Release("release", oberth.Step{Name: "publish", Command: "publish"}).DependsOn("build").
		Build()
}
`

func TestInterpretPreservesPipelineContract(t *testing.T) {
	pipeline, err := Interpret(representativeSource, NewContext("demo", "feature/x", "abc123", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(pipeline.Burns) != 4 {
		t.Fatalf("burn count = %d, want 4", len(pipeline.Burns))
	}
	vet := pipeline.Burns[0].Steps[0]
	if got := strings.Join(vet.Args, " "); got != "vet -tags=ci ./..." {
		t.Fatalf("vet args = %q", got)
	}
	if got := pipeline.Burns[1].Steps[0].Timeout; got != 2*time.Minute {
		t.Fatalf("step timeout = %s", got)
	}
	if got := pipeline.Burns[2].Steps[0].Env["GOOS"]; got != "linux" {
		t.Fatalf("GOOS = %q", got)
	}
	if pipeline.Burns[3].Type != Release {
		t.Fatalf("release type = %s", pipeline.Burns[3].Type)
	}
}

func TestInterpretRejectsImportsOutsideSDK(t *testing.T) {
	source := `//go:build ignore

package main
import (
	"oberth"
	"os"
)
func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	_ = os.Getenv("PATH")
	return oberth.New().Retrograde("test", ctx.Go.Test("./...")).Build()
}`
	_, err := Interpret(source, NewContext("demo", "main", "abc", ""))
	if err == nil || !strings.Contains(err.Error(), `import "os" is not allowed`) {
		t.Fatalf("error = %v", err)
	}
}

func TestInterpretTimeoutIncludesPipelineInvocation(t *testing.T) {
	source := `//go:build ignore

package main
import "oberth"
func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	for {}
}`
	started := time.Now()
	_, err := interpretWithTimeout(source, NewContext("demo", "main", "abc", ""), 30*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout returned after %s", elapsed)
	}
}

func TestInterpretRecoversPipelinePanic(t *testing.T) {
	source := `//go:build ignore

package main
import "oberth"
func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	panic("bad pipeline")
}`
	_, err := Interpret(source, NewContext("demo", "main", "abc", ""))
	if err == nil || !strings.Contains(err.Error(), "bad pipeline") {
		t.Fatalf("error = %v", err)
	}
}

func TestInterpretRequiresBuildConstraintAndSignature(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{
			name: "missing ignore constraint",
			source: `package main
import "oberth"
func Pipeline(ctx *oberth.Context) oberth.Pipeline { return oberth.New().Build() }`,
			want: "//go:build ignore",
		},
		{
			name: "wrong signature",
			source: `//go:build ignore

package main
import "oberth"
func Pipeline() oberth.Pipeline { return oberth.New().Build() }`,
			want: "must have signature",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Interpret(test.source, NewContext("demo", "main", "abc", ""))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestBuilderBuildReturnsDeepCopy(t *testing.T) {
	args := []string{"-c", "true"}
	env := map[string]string{"MODE": "original"}
	deps := []string{"lint"}
	builder := New().
		Retrograde("lint", Step{Name: "lint", Command: "true"}).
		Retrograde("test", Step{Name: "test", Command: "sh", Args: args, Env: env}).DependsOn(deps...)
	first := builder.Build()

	args[0] = "changed"
	env["MODE"] = "changed"
	deps[0] = "changed"
	second := builder.DependsOn("other").Build()

	got := first.Burns[1]
	if !slices.Equal(got.DependsOn, []string{"lint"}) || !slices.Equal(got.Steps[0].Args, []string{"-c", "true"}) || got.Steps[0].Env["MODE"] != "original" {
		t.Fatalf("first build mutated: %#v", got)
	}
	if first.Burns[1].DependsOn[0] == second.Burns[1].DependsOn[0] {
		t.Fatalf("builds unexpectedly alias dependencies: first=%v second=%v", first.Burns[1].DependsOn, second.Burns[1].DependsOn)
	}
}

func TestValidateAndOrderBurns(t *testing.T) {
	pipeline := Pipeline{Burns: []Burn{
		{Name: "build", Type: Prograde, DependsOn: []string{"test"}, Steps: []Step{{Name: "build", Command: "true", Args: []string{"build"}}}},
		{Name: "lint", Type: Retrograde, Steps: []Step{{Name: "lint", Command: "true", Args: []string{"lint"}}}},
		{Name: "test", Type: Retrograde, DependsOn: []string{"lint"}, Steps: []Step{{Name: "test", Command: "true", Args: []string{"test"}}}},
	}}
	ordered, err := OrderedBurns(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{ordered[0].Name, ordered[1].Name, ordered[2].Name}
	if !slices.Equal(got, []string{"lint", "test", "build"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestValidateRejectsUnsafeAndAmbiguousDefinitions(t *testing.T) {
	tests := []struct {
		name string
		p    Pipeline
		want string
	}{
		{
			name: "cycle",
			p: Pipeline{Burns: []Burn{
				{Name: "a", Type: Retrograde, DependsOn: []string{"b"}, Steps: []Step{{Name: "a", Command: "true"}}},
				{Name: "b", Type: Retrograde, DependsOn: []string{"a"}, Steps: []Step{{Name: "b", Command: "true"}}},
			}},
			want: "cycle",
		},
		{
			name: "duplicate steps",
			p: Pipeline{Burns: []Burn{{Name: "test", Type: Retrograde, Steps: []Step{
				{Name: "same", Command: "true"}, {Name: "same", Command: "true"},
			}}}},
			want: "duplicate step name",
		},
		{
			name: "log marker injection",
			p:    Pipeline{Burns: []Burn{{Name: "bad/name", Type: Retrograde, Steps: []Step{{Name: "test", Command: "true"}}}}},
			want: "invalid name",
		},
		{
			name: "invalid timeout",
			p:    Pipeline{Burns: []Burn{{Name: "test", Type: Retrograde, Steps: []Step{{Name: "test", Command: "true", Timeout: -time.Second}}}}},
			want: "timeout must be greater than zero",
		},
		{
			name: "ci depends on release",
			p: Pipeline{Burns: []Burn{
				{Name: "release", Type: Release, Steps: []Step{{Name: "release", Command: "true"}}},
				{Name: "test", Type: Retrograde, DependsOn: []string{"release"}, Steps: []Step{{Name: "test", Command: "true"}}},
			}},
			want: "cannot depend on release burn",
		},
		{
			// The #797 defect class: burns named for different platforms whose
			// steps are byte-identical native builds.
			name: "identical invocation across burns in one class",
			p: Pipeline{Burns: []Burn{
				{Name: "build-amd64", Type: Prograde, Steps: []Step{(&GoSDK{}).Build(".")}},
				{Name: "build-darwin-arm64", Type: Prograde, Steps: []Step{(&GoSDK{}).Build(".")}},
			}},
			want: "identical command, arguments, and environment",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := Validate(test.p)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateStepIdentityScopes(t *testing.T) {
	sdk := &GoSDK{}
	t.Run("cross-compile targets are distinct", func(t *testing.T) {
		pipeline := Pipeline{Burns: []Burn{
			{Name: "build-amd64", Type: Prograde, Steps: []Step{sdk.BuildFor("linux", "amd64", ".")}},
			{Name: "build-arm64", Type: Prograde, Steps: []Step{sdk.BuildFor("linux", "arm64", ".")}},
			{Name: "build-darwin-arm64", Type: Prograde, Steps: []Step{sdk.BuildFor("darwin", "arm64", ".")}},
		}}
		if err := Validate(pipeline); err != nil {
			t.Fatalf("BuildFor pipeline rejected: %v", err)
		}
	})
	t.Run("WithEnv differentiation is sufficient", func(t *testing.T) {
		pipeline := Pipeline{Burns: []Burn{
			{Name: "build-amd64", Type: Prograde, Steps: []Step{sdk.Build(".").WithEnv("GOARCH", "amd64")}},
			{Name: "build-arm64", Type: Prograde, Steps: []Step{sdk.Build(".").WithEnv("GOARCH", "arm64")}},
		}}
		if err := Validate(pipeline); err != nil {
			t.Fatalf("WithEnv-differentiated pipeline rejected: %v", err)
		}
	})
	t.Run("release burns may repeat CI verification steps", func(t *testing.T) {
		pipeline := Pipeline{Burns: []Burn{
			{Name: "test", Type: Retrograde, Steps: []Step{sdk.TestRace("./...")}},
			{Name: "release-test", Type: Release, Steps: []Step{sdk.TestRace("./...")}},
		}}
		if err := Validate(pipeline); err != nil {
			t.Fatalf("cross-class repetition rejected: %v", err)
		}
	})
	t.Run("identical steps inside one burn are rejected", func(t *testing.T) {
		pipeline := Pipeline{Burns: []Burn{
			{Name: "build", Type: Prograde, Steps: []Step{
				namedTestStep("one", sdk.Build(".")),
				namedTestStep("two", sdk.Build(".")),
			}},
		}}
		err := Validate(pipeline)
		if err == nil || !strings.Contains(err.Error(), "identical command, arguments, and environment") {
			t.Fatalf("Validate() error = %v, want identical-invocation rejection", err)
		}
	})
}

func namedTestStep(name string, step Step) Step {
	step.Name = name
	return step
}

func TestGoBuildForPinsTargetPlatform(t *testing.T) {
	step := (&GoSDK{}).BuildFor("darwin", "arm64", "./cmd/tool")
	if step.Env["GOOS"] != "darwin" || step.Env["GOARCH"] != "arm64" || step.Env["CGO_ENABLED"] != "0" {
		t.Fatalf("BuildFor env = %v", step.Env)
	}
	if step.Command != "go" || !slices.Equal(step.Args, []string{"build", "./cmd/tool"}) {
		t.Fatalf("BuildFor invocation = %s %v", step.Command, step.Args)
	}
	if err := validateDefinitionName(step.Name); err != nil {
		t.Fatalf("BuildFor step name %q invalid: %v", step.Name, err)
	}
	if !strings.Contains(step.Name, "darwin-arm64") {
		t.Fatalf("BuildFor step name %q does not carry the target", step.Name)
	}
	withArgs := step.WithArgs("-trimpath")
	if !slices.Equal(withArgs.Args, []string{"build", "-trimpath", "./cmd/tool"}) {
		t.Fatalf("BuildFor WithArgs insertion = %v", withArgs.Args)
	}
}

func TestValidateEnforcesCompleteTerminationSummaryBudgetPerTrigger(t *testing.T) {
	pipeline := Pipeline{Burns: []Burn{
		terminationBudgetBurn("b", Retrograde),
		terminationBudgetBurn("r", Release),
	}}
	if err := Validate(pipeline); err != nil {
		t.Fatalf("exact-budget pipeline rejected: %v", err)
	}

	for _, test := range []struct {
		name      string
		burnIndex int
		trigger   Trigger
	}{
		{name: "ci", burnIndex: 0, trigger: TriggerCI},
		{name: "release", burnIndex: 1, trigger: TriggerRelease},
	} {
		t.Run(test.name, func(t *testing.T) {
			overBudget := pipeline.Clone()
			last := len(overBudget.Burns[test.burnIndex].Steps) - 1
			overBudget.Burns[test.burnIndex].Steps[last].Name += "x"
			err := Validate(overBudget)
			want := fmt.Sprintf("%s termination summary requires 4097 bytes, maximum is 4096", test.trigger)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Validate() error = %v, want %q", err, want)
			}
		})
	}
}

func TestSelectBurnsSeparatesCIAndRelease(t *testing.T) {
	pipeline, err := Interpret(representativeSource, NewContext("demo", "main", "abc", "v1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	ci, err := Select(pipeline.Clone(), TriggerCI)
	if err != nil {
		t.Fatal(err)
	}
	release, err := Select(pipeline.Clone(), TriggerRelease)
	if err != nil {
		t.Fatal(err)
	}
	if len(ci.Burns) != 3 || len(release.Burns) != 1 || release.Burns[0].Name != "release" {
		t.Fatalf("ci=%v release=%v", burnNames(ci.Burns), burnNames(release.Burns))
	}
	if len(release.Burns[0].DependsOn) != 0 {
		t.Fatalf("external CI dependency was not satisfied during selection: %v", release.Burns[0].DependsOn)
	}
	ci.Burns[0].Name = "mutated"
	if pipeline.Burns[0].Name == "mutated" {
		t.Fatal("selection aliased source pipeline")
	}
}

func burnNames(burns []Burn) []string {
	names := make([]string, 0, len(burns))
	for _, burn := range burns {
		names = append(names, burn.Name)
	}
	return names
}

func terminationBudgetBurn(name string, burnType BurnType) Burn {
	steps := make([]Step, 20)
	for index := range steps {
		nameBytes := 52
		if index == len(steps)-1 {
			nameBytes = 47
		}
		suffix := fmt.Sprintf("%02d", index)
		steps[index] = Step{
			Name:    strings.Repeat("s", nameBytes-len(suffix)) + suffix,
			Command: "true",
			// Arguments keep every step's invocation identity distinct; the
			// termination summary budget under test counts only names.
			Args: []string{suffix},
		}
	}
	return Burn{Name: name, Type: burnType, Steps: steps}
}
