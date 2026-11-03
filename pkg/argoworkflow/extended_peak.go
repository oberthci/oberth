package argoworkflow

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

func containerExtendedDemand(c, defaults *corev1.Container) (map[string]int64, error) {
	names := map[corev1.ResourceName]bool{}
	for _, container := range []*corev1.Container{c, defaults} {
		if container == nil {
			continue
		}
		for _, list := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
			for name := range list {
				if name != corev1.ResourceCPU && name != corev1.ResourceMemory && name != corev1.ResourceEphemeralStorage {
					names[name] = true
				}
			}
		}
	}
	var out map[string]int64
	for name := range names {
		request, ok := c.Resources.Requests[name]
		if !ok && defaults != nil {
			request, ok = defaults.Resources.Requests[name]
		}
		limit, limited := c.Resources.Limits[name]
		if !limited && defaults != nil {
			limit, limited = defaults.Resources.Limits[name]
		}
		count, integer := request.AsInt64()
		if !strings.Contains(string(name), "/") || !ok || !limited || !integer || count < 1 || count > 1_000_000 || request.Cmp(limit) != 0 {
			return nil, errPodGraph
		}
		if out == nil {
			out = map[string]int64{}
		}
		out[string(name)] = count
	}
	return out, nil
}

func mergeExtended(a, b map[string]int64, combine func(int64, int64) int64) map[string]int64 {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]int64, len(a)+len(b))
	for name, count := range a {
		out[name] = count
	}
	for name, count := range b {
		out[name] = combine(out[name], count)
	}
	return out
}
func sumExtended(a, b map[string]int64) map[string]int64 {
	return mergeExtended(a, b, func(a, b int64) int64 { return a + b })
}
func maxExtended(a, b map[string]int64) map[string]int64 {
	return mergeExtended(a, b, func(a, b int64) int64 { return max(a, b) })
}
func scaleExtended(values map[string]int64, count int64) map[string]int64 {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]int64, len(values))
	for name, value := range values {
		out[name] = value * count
	}
	return out
}
func extendedVectors(demands []PodDemand) map[string][]int64 {
	var result map[string][]int64
	for index, demand := range demands {
		for name, count := range demand.ExtendedResources {
			if result == nil {
				result = map[string][]int64{}
			}
			if result[name] == nil {
				result[name] = make([]int64, len(demands))
			}
			result[name][index] = count
		}
	}
	return result
}
func peakExtended(reach [][]bool, demands []PodDemand) map[string]int64 {
	var result map[string]int64
	for name, values := range extendedVectors(demands) {
		if result == nil {
			result = map[string]int64{}
		}
		result[name] = maxWeightedAntichain(reach, values)
	}
	return result
}
func limitExtendedPeak(peak map[string]int64, demands []PodDemand, limit int) map[string]int64 {
	if len(peak) == 0 {
		return nil
	}
	vectors := extendedVectors(demands)
	result := make(map[string]int64, len(peak))
	for name, count := range peak {
		result[name] = min(count, largestPodRequests(vectors[name], limit))
	}
	return result
}
