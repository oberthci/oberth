package main

import "encoding/json"

func parallelMarks(steps []any) []string {
	marks := make([]string, len(steps))
	for start := 0; start < len(steps); {
		group, end := stepGroup(steps[start]), start+1
		for group > 0 && end < len(steps) && stepGroup(steps[end]) == group {
			end++
		}
		if group > 0 && end-start > 1 {
			marks[start], marks[end-1] = "┌ ", "└ "
			for index := start + 1; index < end-1; index++ {
				marks[index] = "│ "
			}
		}
		start = end
	}
	return marks
}

func stepGroup(value any) int64 {
	step, _ := value.(map[string]any)
	number, _ := step["parallel_group"].(json.Number)
	group, _ := number.Int64()
	return group
}
