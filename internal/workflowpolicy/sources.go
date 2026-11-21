package workflowpolicy

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/oberthci/oberth/internal/reponame"
)

const (
	MaxSourcePolicyBytes = 64 << 10
	MaxSourceConsumers   = 256
	MaxSourceProducers   = 64
)

// SourcePolicy is administrator policy: for each consuming repository, the
// producing repositories whose exact commits its pipelines may pin. A pair
// that is not listed is denied, and a repository pinning its own older commit
// is a pair like any other.
type SourcePolicy map[string][]string

// ParseSourcePolicy decodes the JSON policy and bounds it; empty input is the
// empty policy. The server and the installer both read it here.
func ParseSourcePolicy(raw []byte) (SourcePolicy, error) {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if len(raw) > MaxSourcePolicyBytes {
		return nil, fmt.Errorf("policy exceeds %d bytes", MaxSourcePolicyBytes)
	}
	var policy SourcePolicy
	if err := json.Unmarshal(raw, &policy); err != nil {
		return nil, fmt.Errorf("policy is not a JSON object of repository lists: %w", err)
	}
	if len(policy) > MaxSourceConsumers {
		return nil, fmt.Errorf("policy names more than %d consumers", MaxSourceConsumers)
	}
	for _, consumer := range slices.Sorted(maps.Keys(policy)) {
		if err := validPair(consumer, policy[consumer]); err != nil {
			return nil, err
		}
	}
	return policy, nil
}

func validPair(consumer string, producers []string) error {
	if _, err := reponame.ParseRepository(consumer); err != nil {
		return fmt.Errorf("consumer %q is not a canonical repository", consumer)
	}
	if len(producers) > MaxSourceProducers {
		return fmt.Errorf("consumer %q names more than %d producers", consumer, MaxSourceProducers)
	}
	for _, producer := range producers {
		if _, err := reponame.ParseRepository(producer); err != nil {
			return fmt.Errorf("producer %q of %q is not a canonical repository", producer, consumer)
		}
	}
	return nil
}

// Allows reports whether the consumer's pipelines may pin the producer.
func (p SourcePolicy) Allows(consumer, producer string) bool {
	return slices.Contains(p[consumer], producer)
}

// Pairs lists the distinct allowed pairs, sorted, each in the
// CONSUMER=PRODUCER form the installer's --source-pin takes.
func (p SourcePolicy) Pairs() []string {
	var pairs []string
	for consumer, producers := range p {
		for _, producer := range producers {
			pairs = append(pairs, consumer+"="+producer)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(pairs)))
}
