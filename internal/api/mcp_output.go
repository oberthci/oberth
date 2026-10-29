package api

// toolOutputSchema describes the object boundary for every registered tool.
// Object fields remain extensible. Collection tools additionally promise their
// named array, including an empty array when no records match.
func toolOutputSchema(name string) map[string]any {
	schema := map[string]any{"type": "object"}
	collection := ""
	switch name {
	case "promote_grant_list", "kubevirt_grant_list":
		collection = "grants"
	case "run_list":
		collection = "runs"
	case "repo_list":
		collection = "repositories"
	case "promotion_list":
		collection = "promotions"
	case "issue_list":
		collection = "issues"
	case "issue_get_many", "issue_close_many":
		collection = "results"
	case "artifacts":
		collection = "artifacts"
	case "schedule_list":
		collection = "schedules"
	case "uplink_list":
		collection = "uplinks"
	case "audit_list":
		collection = "entries"
	case "access_list":
		schema["anyOf"] = []any{toolCollectionSchema("grants"), toolCollectionSchema("declarations")}
	case "issue_delete":
		schema["properties"] = map[string]any{"id": map[string]any{"type": "integer"}, "deleted": map[string]any{"type": "boolean", "enum": []bool{true}}}
		schema["required"] = []string{"id", "deleted"}
	}
	if collection != "" {
		return toolCollectionSchema(collection)
	}
	return schema
}

func toolCollectionSchema(field string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{field: map[string]any{"type": "array", "items": map[string]any{"type": "object"}}},
		"required":   []string{field},
	}
}
