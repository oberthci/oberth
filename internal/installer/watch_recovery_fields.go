package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func decodeWatchTrie(raw []byte) (map[string]any, error) {
	if len(raw) > 65536 || !uniqueWatchJSON(raw) {
		return nil, errors.New("invalid legacy field trie")
	}
	var tree map[string]any
	if json.Unmarshal(raw, &tree) != nil || len(tree) == 0 {
		return nil, errors.New("empty legacy field trie")
	}
	nodes := 0
	var walk func(map[string]any, int) bool
	walk = func(t map[string]any, depth int) bool {
		if depth > 32 {
			return false
		}
		for key, value := range t {
			nodes++
			if nodes > 4096 || len(key) > 1024 {
				return false
			}
			child, ok := value.(map[string]any)
			if !ok {
				return false
			}
			if key == "." {
				if len(child) != 0 {
					return false
				}
				continue
			}
			if strings.HasPrefix(key, "f:") {
				if len(key) == 2 {
					return false
				}
			} else if strings.HasPrefix(key, "k:") {
				var id map[string]string
				if !uniqueWatchJSON([]byte(key[2:])) || json.Unmarshal([]byte(key[2:]), &id) != nil || len(id) == 0 {
					return false
				}
				for k, v := range id {
					if k == "" || v == "" {
						return false
					}
				}
				if id["name"] == "fetch-token" && key != watchArgsTriePath[4] {
					return false
				}
			} else if strings.HasPrefix(key, "v:") {
				var setValue any
				if !uniqueWatchJSON([]byte(key[2:])) || json.Unmarshal([]byte(key[2:]), &setValue) != nil {
					return false
				}
			} else {
				return false
			}
			if !walk(child, depth+1) {
				return false
			}
		}
		return true
	}
	if !walk(tree, 0) {
		return nil, errors.New("unapproved legacy field trie shape")
	}
	return tree, nil
}

func ownsWatchArgs(tree map[string]any) (bool, error) {
	node := tree
	for i, key := range watchArgsTriePath {
		if len(node) == 0 {
			return false, errors.New("atomic ancestor owns watch args")
		}
		value, ok := node[key]
		if !ok {
			return false, nil
		}
		child, ok := value.(map[string]any)
		if !ok {
			return false, errors.New("invalid watch args ownership")
		}
		if i == len(watchArgsTriePath)-1 {
			if len(child) != 0 {
				return false, errors.New("watch args field is not an atomic leaf")
			}
			return true, nil
		}
		node = child
	}
	return false, nil
}

// There is exactly one approved owner and one permissible subtraction.
// No field-manager name supplied by a plan can widen this operation.
func watchArgsHandoff(entries []metav1.ManagedFieldsEntry) (int, []metav1.ManagedFieldsEntry, error) {
	if len(entries) == 0 || len(entries) > 128 {
		return -1, nil, errors.New("watch ownership entry count differs")
	}
	index := -1
	legacy := 0
	seen := map[string]bool{}
	next := make([]metav1.ManagedFieldsEntry, len(entries))
	for i, e := range entries {
		if e.Manager == "" || e.FieldsType != "FieldsV1" || e.FieldsV1 == nil || (e.Operation != metav1.ManagedFieldsOperationApply && e.Operation != metav1.ManagedFieldsOperationUpdate) {
			return -1, nil, errors.New("unknown watch ownership entry")
		}
		id := e.Manager + "\x00" + string(e.Operation) + "\x00" + e.APIVersion + "\x00" + e.Subresource
		if seen[id] {
			return -1, nil, errors.New("duplicate watch ownership entry")
		}
		seen[id] = true
		tree, err := decodeWatchTrie(e.FieldsV1.Raw)
		if err != nil {
			return -1, nil, err
		}
		owns, err := ownsWatchArgs(tree)
		if err != nil {
			return -1, nil, err
		}
		if e.Manager == watchLegacyManager {
			legacy++
			if e.Operation != metav1.ManagedFieldsOperationUpdate || e.APIVersion != "apps/v1" || e.Subresource != "" {
				return -1, nil, errors.New("legacy watch owner identity differs")
			}
		}
		if owns {
			if e.Manager != watchLegacyManager || index != -1 {
				return -1, nil, errors.New("shared or foreign watch args ownership")
			}
			index = i
		}
		next[i] = *e.DeepCopy()
	}
	if legacy != 1 || index < 0 {
		return -1, nil, errors.New("sole legacy watch args owner not established")
	}
	tree, _ := decodeWatchTrie(next[index].FieldsV1.Raw)
	node := tree
	for _, key := range watchArgsTriePath[:len(watchArgsTriePath)-1] {
		node = node[key].(map[string]any)
	}
	delete(node, "f:args")
	// Keep all ancestors, including membership dots and sibling name fields.
	if len(node) == 0 {
		return -1, nil, errors.New("watch handoff would leave an empty ownership ancestor")
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		return -1, nil, errors.New("cannot encode watch handoff")
	}
	next[index].FieldsV1 = &metav1.FieldsV1{Raw: raw}
	return index, next, nil
}

func watchLegacyConflictMessage(entries []metav1.ManagedFieldsEntry) string {
	for _, e := range entries {
		if e.Manager == watchLegacyManager {
			// Kubernetes BuildManagerIdentifier clears Time before constructing
			// an apply conflict. Complete managedFields timestamps remain bound
			// independently by observation, metadata CAS and reply validation.
			return fmt.Sprintf("conflict with %q using %s", e.Manager, e.APIVersion)
		}
	}
	return ""
}
func sameWatchManaged(a, b []metav1.ManagedFieldsEntry) bool {
	encode := func(entries []metav1.ManagedFieldsEntry) ([][]byte, error) {
		rows := make([][]byte, len(entries))
		for i, e := range entries {
			raw, err := json.Marshal(e)
			if err != nil {
				return nil, err
			}
			var canonical any
			if json.Unmarshal(raw, &canonical) != nil {
				return nil, errors.New("invalid entry")
			}
			rows[i], err = json.Marshal(canonical)
			if err != nil {
				return nil, err
			}
		}
		sort.Slice(rows, func(i, j int) bool { return bytes.Compare(rows[i], rows[j]) < 0 })
		return rows, nil
	}
	x, e := encode(a)
	y, f := encode(b)
	if e != nil || f != nil || len(x) != len(y) {
		return false
	}
	for i := range x {
		if !bytes.Equal(x[i], y[i]) {
			return false
		}
	}
	return true
}
func watchCompleteMetadata(object any) (metav1.ObjectMeta, error) {
	switch x := object.(type) {
	case *appsv1.Deployment:
		return *x.ObjectMeta.DeepCopy(), nil
	case *corev1.ServiceAccount:
		return *x.ObjectMeta.DeepCopy(), nil
	case *corev1.ConfigMap:
		return *x.ObjectMeta.DeepCopy(), nil
	default:
		return metav1.ObjectMeta{}, errors.New("unsupported watch metadata")
	}
}

func sameWatchCompleteMetadata(a, b metav1.ObjectMeta) bool {
	if !sameWatchManaged(a.ManagedFields, b.ManagedFields) {
		return false
	}
	a.ManagedFields, b.ManagedFields = nil, nil
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && sameWatchJSON(x, y)
}

func watchMetadataCAS(v watchObserved, index int) ([]byte, error) {
	metadata, err := watchCompleteMetadata(v.meta)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/metadata/managedFields/%d/fieldsV1", index)
	for _, key := range watchArgsTriePath {
		path += "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
	}
	return json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(v.meta.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": v.meta.GetResourceVersion()}, {"op": "test", "path": "/metadata", "value": metadata}, {"op": "remove", "path": path}})
}
