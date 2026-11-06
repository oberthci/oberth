package argojob

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Preserve the measured Pod spans, including censored attempts, separately
// from the deadline. Container runtime is not a substitute for a Pod span.
func checkOberthDeadlineFloor(seconds int, comment string) error {
	if strings.Contains(comment, "observed span unknown") {
		match := regexp.MustCompile(`provisional previous (\d+)s cap \+ 120s for contention`).FindStringSubmatch(comment)
		if match == nil {
			return fmt.Errorf("unknown span needs an explicit provisional cap")
		}
		previous, _ := strconv.Atoi(match[1])
		if seconds < previous+120 {
			return fmt.Errorf("deadline %ds below provisional %ds contention floor", seconds, previous+120)
		}
		return nil
	}
	basis, _, hasFloor := strings.Cut(comment, "; floor observed+120s for contention")
	if !hasFloor {
		return fmt.Errorf("missing contention floor rule")
	}
	spans := regexp.MustCompile(`(\d+(?:\.\d+)?)s\b`).FindAllStringSubmatch(basis, -1)
	if len(spans) == 0 {
		return fmt.Errorf("missing recorded Pod span")
	}
	var observed float64
	for _, span := range spans {
		value, _ := strconv.ParseFloat(span[1], 64)
		observed = math.Max(observed, value)
	}
	floor := int(math.Ceil(math.Max(1.5*observed, observed+120)))
	if seconds < floor {
		return fmt.Errorf("deadline %ds below %ds contention floor", seconds, floor)
	}
	return nil
}

func TestOberthDeadlineRemeasureFloor(t *testing.T) {
	for _, file := range []string{"build.yaml", "release.yaml"} {
		t.Run(file, func(t *testing.T) {
			var workflow struct {
				Spec struct {
					Templates []struct {
						Name     string    `yaml:"name"`
						Deadline yaml.Node `yaml:"activeDeadlineSeconds"`
					} `yaml:"templates"`
				} `yaml:"spec"`
			}
			if err := yaml.Unmarshal(loadPipeline(t, file), &workflow); err != nil {
				t.Fatal(err)
			}
			for _, template := range workflow.Spec.Templates {
				if template.Deadline.Kind == 0 {
					continue
				}
				seconds, err := strconv.Atoi(template.Deadline.Value)
				if err != nil {
					t.Fatal(err)
				}
				comment := template.Deadline.LineComment
				// Larger retained bounds without Pod-span evidence are not
				// re-estimated. Every short cap needs an auditable basis.
				if seconds >= 360 && !strings.Contains(comment, "remeasure:") {
					continue
				}
				if err := checkOberthDeadlineFloor(seconds, comment); err != nil {
					t.Errorf("%s: %v", template.Name, err)
				}
			}
		})
	}
}

func TestOberthDeadlineFloorRejectsInsufficientSlack(t *testing.T) {
	for _, tc := range []struct {
		name, basis string
		floor       int
	}{
		{"short", "observed 62s pod span x1.5", 182},
		{"rounded", "observed 62.1s pod span x1.5", 183},
		{"censored maximum", "max(60s censored cap, 63s pod span) x1.5", 183},
		{"crossover", "observed 240s pod span x1.5", 360},
		{"long", "censored 600s pod deadline x1.5", 900},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comment := "# remeasure: " + tc.basis + "; floor observed+120s for contention"
			if err := checkOberthDeadlineFloor(tc.floor, comment); err != nil {
				t.Fatal(err)
			}
			if err := checkOberthDeadlineFloor(tc.floor-1, comment); err == nil {
				t.Fatal("accepted a deadline below the floor")
			}
		})
	}
	comment := "# remeasure: observed span unknown; provisional previous 60s cap + 120s for contention"
	if err := checkOberthDeadlineFloor(180, comment); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		seconds int
		comment string
	}{
		{179, comment}, {180, ""}, {180, "# remeasure: observed span unknown"},
		{180, "# remeasure: no span; floor observed+120s for contention"},
	} {
		if err := checkOberthDeadlineFloor(tc.seconds, tc.comment); err == nil {
			t.Errorf("accepted missing evidence or insufficient slack: %+v", tc)
		}
	}
}
