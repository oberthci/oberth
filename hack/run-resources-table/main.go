// Generates an anonymous offline table with the runtime report policy.
package main

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
)

type row struct {
	Repo          string `json:"repo"`
	Run           string `json:"run"`
	Burn          string
	Step          string
	Fragment      bool `json:"fragment"`
	MeasuredUsage []model.StepResourceUsage
}
type captured struct {
	declaration measured.Declaration
	at          time.Time
}

var repoLabel = regexp.MustCompile(`^repo-[0-9]{2}$`)
var stepLabel = regexp.MustCompile(`^step-[0-9a-f]{8,64}$`)
var runLabel = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

func main() {
	input := flag.String("input", "", "anonymised step JSONL.gz")
	output := flag.String("output", "docs/measured-requests-table.md", "output Markdown")
	flag.Parse()
	if e := run(*input, *output); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func run(input, output string) error {
	f, e := os.Open(input) // #nosec G304 -- explicit operator-supplied anonymous offline capture.
	if e != nil {
		return e
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	z, e := gzip.NewReader(io.TeeReader(f, hash))
	if e != nil {
		return e
	}
	defer func() { _ = z.Close() }()
	scanner := bufio.NewScanner(z)
	scanner.Buffer(make([]byte, 65536), 2<<20)
	groups := map[string][]measured.Observation{}
	declarations := map[string]map[string]captured{}
	runs := map[string]bool{}
	ambiguous := map[string]int{}
	rowCount := 0
	for scanner.Scan() {
		var r row
		if e := json.Unmarshal(scanner.Bytes(), &r); e != nil {
			return errors.New("invalid anonymous capture JSON")
		}
		if !repoLabel.MatchString(r.Repo) || !stepLabel.MatchString(r.Burn) || !stepLabel.MatchString(r.Step) || !runLabel.MatchString(r.Run) {
			return errors.New("anonymous repo-NN, step hashes and run hash required")
		}
		rowCount++
		runs[r.Run] = true
		if declarations[r.Repo] == nil {
			declarations[r.Repo] = map[string]captured{}
		}
		owner := r.Repo
		if r.Fragment {
			owner = ""
		}
		d := measured.Declaration{Burn: r.Burn, Template: r.Step, OwnerRepository: owner, Resources: map[string]map[string]string{"requests": {}, "limits": {}}}
		// Version 1 had only main-container scope. This export removes v2
		// container names and all incarnation IDs. Array order and numeric
		// diagnostics are explicitly not a guaranteed main/attempt binding.
		var v model.StepResourceUsage
		unambiguous := len(r.MeasuredUsage) == 1 && r.MeasuredUsage[0].Version == 1
		if unambiguous {
			v = r.MeasuredUsage[0]
		} else {
			v = model.StepResourceUsage{Version: 2, Source: "kubelet-cadvisor", ContainerKind: "app", PodKind: "workflow"}
			ambiguous[r.Repo+"\x00"+r.Burn+"\x00"+r.Step]++
		}
		// Anonymous grouping keys are not reconstructed execution evidence.
		// All offline rows retain the ambiguity flag: original incarnation
		// identity and numeric diagnostic pairing were removed.
		v.RunID = r.Run
		v.Burn = r.Burn
		v.Step = r.Step
		v.TemplateName = r.Step
		v.ContainerName = "main"
		v.PodUID = r.Run + "/" + r.Burn + "/" + r.Step
		v.ContainerID = v.PodUID
		if unambiguous {
			for _, q := range []struct {
				name string
				n    *int64
			}{{"memory", v.MemoryRequestBytes}, {"ephemeral-storage", v.EphemeralRequestBytes}} {
				if q.n != nil {
					d.Resources["requests"][q.name] = resource.NewQuantity(*q.n, resource.BinarySI).String()
				}
			}
			if v.CPURequestNanoCores != nil {
				d.Resources["requests"]["cpu"] = resource.NewMilliQuantity((*v.CPURequestNanoCores+999999)/1000000, resource.DecimalSI).String()
			}
		}
		groups[r.Repo] = append(groups[r.Repo], measured.Observation{Usage: v, Template: r.Step, Owner: owner, Ambiguous: true})
		k := r.Burn + "\x00" + r.Step
		previous, ok := declarations[r.Repo][k]
		if !ok || v.LastObservedAt.After(previous.at) {
			declarations[r.Repo][k] = captured{d, v.LastObservedAt}
		}
	}
	if e := scanner.Err(); e != nil {
		return e
	}
	ids := make([]string, 0, len(declarations))
	for id := range declarations {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var b strings.Builder
	fmt.Fprintf(&b, "# Anonymous captured resources against suggestions\n\nGenerated only from `step-usage-anon.jsonl.gz`, using `measured.Build`, the same\naggregation and suggestion code as `run_resources`. Input SHA-256:\n`%s`. %d rows, %d distinct runs, %d repositories.\n\n", hex.EncodeToString(hash.Sum(nil)), rowCount, len(runs), len(ids))
	b.WriteString("Only supplied anonymous repository labels and step hashes are used. No refs or\noriginal names are reconstructed. Container names and incarnation IDs were\nremoved; array order and numeric termination data do not guarantee a main/attempt\npairing. Ambiguous rows receive no suggestion.\n\nVersion 1 records had main-container-only scope. Their captured requests can\nbe shown, but missing identity, duration and limit pairing prevent suggestions.\nVersion 2 main-container values remain unknown. Polls count only attributable\nversion 1 records. Today means the newest attributable captured request, not\na current git declaration. Fragment ownership is not recoverable. Failed rows\nare included for review. CPU is millicores; memory/storage are MiB; time is seconds.\n\n| Repo | Burn / step hash | Runs / attributable polls | Today CPU / memory | Suggested CPU / memory request / memory limit | Ephemeral request / limit | Deadline | Flags / no suggestion |\n| --- | --- | ---: | ---: | ---: | ---: | ---: | --- |\n")
	for _, id := range ids {
		declared := make([]measured.Declaration, 0, len(declarations[id]))
		for _, d := range declarations[id] {
			declared = append(declared, d.declaration)
		}
		for _, s := range measured.Build(groups[id], declared) {
			todayCPU, todayMem := "-", "-"
			if s.Declared != nil {
				if q, e := resource.ParseQuantity(s.Declared.Resources["requests"]["cpu"]); e == nil {
					todayCPU = fmt.Sprint(q.MilliValue())
				}
				if q, e := resource.ParseQuantity(s.Declared.Resources["requests"]["memory"]); e == nil {
					todayMem = fmt.Sprint(q.Value() >> 20)
				}
			}
			notes := strings.Join(s.NoSuggestion, "; ") + "; anonymous incarnation/diagnostic pairing is ambiguous"
			if n := ambiguous[id+"\x00"+s.Burn+"\x00"+s.Template]; n > 0 {
				notes += fmt.Sprintf("; %d rows lack attributable main-container scope", n)
			}
			if s.OwnerRepository == "" {
				notes += "; fragment owner unknown"
			}
			if s.Suggested != nil {
				return errors.New("anonymous ambiguity unexpectedly produced a suggestion")
			}
			fmt.Fprintf(&b, "| %s | %s / %s | %d / %d | %s / %s | - | - | - | %s |\n", id, s.Burn, s.Template, s.Runs, s.Samples, todayCPU, todayMem, strings.Trim(notes, "; "))
		}
	}
	b.WriteString("\nNo complete, attributable population supports a suggestion. Total CPU and\nmemory saved or added are **unknown**, not zero. Existing git declarations are\nunchanged. Re-run the live tool after release with its identity-bound records.\n")
	fmt.Fprintf(&b, "\n%s\n", measured.Retention)
	return os.WriteFile(output, []byte(b.String()), 0600)
}
