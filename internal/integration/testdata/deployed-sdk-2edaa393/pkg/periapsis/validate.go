package periapsis

import (
	"errors"
	"fmt"
	"strings"
)

const (
	maxDefinitionNameBytes = 63
	maxBurns               = 128
	maxStepsPerBurn        = 256
	maxArgumentsPerStep    = 512
	maxEnvironmentPerStep  = 256

	// The fixed portion uses the longest runner-emitted status, exit code, and
	// UTC RFC3339Nano timestamps. Valid definition names need no JSON escaping.
	maxTerminationStepResultFixedBytes = len(`{"burn":"","step":"","status":"timed_out","exit_code":255,"started_at":"9999-12-31T23:59:59.999999999Z","finished_at":"9999-12-31T23:59:59.999999999Z"}`)
)

// MaxTerminationSummaryBytes is Kubernetes' per-container termination-message
// limit. Pipeline admission guarantees that every selected step result fits.
const MaxTerminationSummaryBytes = 4096

// Validate checks structural, naming, dependency, and workload-hint safety.
func Validate(p Pipeline) error {
	if len(p.Burns) == 0 {
		return errors.New("pipeline has no burns")
	}
	if len(p.Burns) > maxBurns {
		return fmt.Errorf("pipeline has %d burns, maximum is %d", len(p.Burns), maxBurns)
	}

	names := make(map[string]BurnType, len(p.Burns))
	var ciSummary, releaseSummary terminationSummaryBudget
	var errs []error
	for index, burn := range p.Burns {
		if err := validateDefinitionName(burn.Name); err != nil {
			errs = append(errs, fmt.Errorf("burn %d: %w", index, err))
		}
		if _, exists := names[burn.Name]; exists {
			errs = append(errs, fmt.Errorf("burn %q: duplicate name", burn.Name))
		}
		names[burn.Name] = burn.Type
		if burn.Type != Retrograde && burn.Type != Prograde && burn.Type != Escaped {
			errs = append(errs, fmt.Errorf("burn %q: unknown burn type %d", burn.Name, burn.Type))
		}
		if len(burn.Steps) == 0 {
			errs = append(errs, fmt.Errorf("burn %q: has no steps", burn.Name))
		}
		if len(burn.Steps) > maxStepsPerBurn {
			errs = append(errs, fmt.Errorf("burn %q: has %d steps, maximum is %d", burn.Name, len(burn.Steps), maxStepsPerBurn))
		}
		stepNames := make(map[string]struct{}, len(burn.Steps))
		for stepIndex, step := range burn.Steps {
			switch burn.Type {
			case Retrograde, Prograde:
				ciSummary.add(burn.Name, step.Name)
			case Escaped:
				releaseSummary.add(burn.Name, step.Name)
			}
			if err := validateDefinitionName(step.Name); err != nil {
				errs = append(errs, fmt.Errorf("burn %q step %d: %w", burn.Name, stepIndex, err))
			}
			if _, exists := stepNames[step.Name]; exists {
				errs = append(errs, fmt.Errorf("burn %q: duplicate step name %q", burn.Name, step.Name))
			}
			stepNames[step.Name] = struct{}{}
			if strings.TrimSpace(step.Command) == "" || strings.ContainsAny(step.Command, "\x00\r\n") {
				errs = append(errs, fmt.Errorf("burn %q step %q: command is empty or contains a control character", burn.Name, step.Name))
			}
			if len(step.Args) > maxArgumentsPerStep {
				errs = append(errs, fmt.Errorf("burn %q step %q: has too many arguments", burn.Name, step.Name))
			}
			for _, argument := range step.Args {
				if strings.ContainsRune(argument, '\x00') {
					errs = append(errs, fmt.Errorf("burn %q step %q: argument contains NUL", burn.Name, step.Name))
					break
				}
			}
			if len(step.Env) > maxEnvironmentPerStep {
				errs = append(errs, fmt.Errorf("burn %q step %q: has too many environment entries", burn.Name, step.Name))
			}
			for key, value := range step.Env {
				if !validEnvironmentName(key) {
					errs = append(errs, fmt.Errorf("burn %q step %q: invalid environment name %q", burn.Name, step.Name, key))
				}
				if strings.ContainsRune(value, '\x00') {
					errs = append(errs, fmt.Errorf("burn %q step %q: environment %q contains NUL", burn.Name, step.Name, key))
				}
			}
			if step.Timeout < 0 {
				errs = append(errs, fmt.Errorf("burn %q step %q: timeout must be greater than zero", burn.Name, step.Name))
			}
		}
	}

	for _, burn := range p.Burns {
		dependencies := make(map[string]struct{}, len(burn.DependsOn))
		for _, dependency := range burn.DependsOn {
			if _, exists := names[dependency]; !exists {
				errs = append(errs, fmt.Errorf("burn %q: dependency %q does not exist", burn.Name, dependency))
			}
			if _, duplicate := dependencies[dependency]; duplicate {
				errs = append(errs, fmt.Errorf("burn %q: duplicate dependency %q", burn.Name, dependency))
			}
			dependencies[dependency] = struct{}{}
			if burn.Type != Escaped && names[dependency] == Escaped {
				errs = append(errs, fmt.Errorf("burn %q: CI burn cannot depend on escaped burn %q", burn.Name, dependency))
			}
		}
	}
	if err := cycleError(p.Burns); err != nil {
		errs = append(errs, err)
	}
	if err := ciSummary.validate(TriggerCI); err != nil {
		errs = append(errs, err)
	}
	if err := releaseSummary.validate(TriggerRelease); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

type terminationSummaryBudget struct {
	steps       int
	objectBytes int
}

func (b *terminationSummaryBudget) add(burn, step string) {
	if b.steps > 0 {
		b.objectBytes++ // comma between array entries
	}
	b.objectBytes += maxTerminationStepResultFixedBytes + len(burn) + len(step)
	b.steps++
}

func (b terminationSummaryBudget) validate(trigger Trigger) error {
	if b.steps == 0 {
		return nil
	}
	required := 2 + b.objectBytes // JSON array brackets
	if required > MaxTerminationSummaryBytes {
		return fmt.Errorf("%s termination summary requires %d bytes, maximum is %d", trigger, required, MaxTerminationSummaryBytes)
	}
	return nil
}

func validateDefinitionName(value string) error {
	if len(value) == 0 || len(value) > maxDefinitionNameBytes {
		return fmt.Errorf("invalid name %q: must contain 1-%d bytes", value, maxDefinitionNameBytes)
	}
	for index, character := range []byte(value) {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
		if index > 0 {
			valid = valid || character == '-' || character == '_' || character == '.'
		}
		if !valid {
			return fmt.Errorf("invalid name %q: use ASCII letters, digits, dots, dashes, or underscores and begin with an alphanumeric", value)
		}
	}
	return nil
}

func validEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range []byte(value) {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character == '_' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func cycleError(burns []Burn) error {
	dependencies := make(map[string][]string, len(burns))
	for _, burn := range burns {
		dependencies[burn.Name] = burn.DependsOn
	}
	const (
		unseen = iota
		visiting
		done
	)
	state := make(map[string]int, len(burns))
	var visit func(string) error
	visit = func(name string) error {
		state[name] = visiting
		for _, dependency := range dependencies[name] {
			switch state[dependency] {
			case visiting:
				return fmt.Errorf("dependency cycle: %s -> %s", name, dependency)
			case unseen:
				if err := visit(dependency); err != nil {
					return err
				}
			}
		}
		state[name] = done
		return nil
	}
	for _, burn := range burns {
		if state[burn.Name] == unseen {
			if err := visit(burn.Name); err != nil {
				return err
			}
		}
	}
	return nil
}
