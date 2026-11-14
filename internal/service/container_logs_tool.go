package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/runlog"
)

func (service *API) callRunLogs(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	var arguments struct {
		ID         string `json:"id"`
		Containers bool   `json:"containers"`
		Raw        bool   `json:"raw"`
		NodeID     string `json:"node_id"`
		Container  string `json:"container"`
		Stage      string `json:"burn"`
		Step       string `json:"step"`
		Pattern    string `json:"pattern"`
		Context    int    `json:"context"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		Tail       bool   `json:"tail"`
		ByteOffset *int64 `json:"byte_offset"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	if strings.TrimSpace(arguments.ID) == "" {
		return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	filter, err := toolLogFilter(arguments.Pattern, arguments.Context, arguments.Offset, arguments.Limit, arguments.Tail)
	if err != nil {
		return nil, err
	}
	if arguments.ByteOffset != nil && *arguments.ByteOffset < 0 {
		return nil, fmt.Errorf("%w: byte_offset must be nonnegative", ErrInvalidInput)
	}
	filter.ByteOffset = arguments.ByteOffset
	filter.Raw = arguments.Raw
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields) // decodeTool already validated the object.
	defaultWindow := true
	for field := range fields {
		switch strings.ToLower(field) {
		case "pattern", "limit", "tail", "offset", "byte_offset":
			defaultWindow = false
		}
	}
	if defaultWindow {
		filter.Tail, filter.Limit = true, runlog.DefaultWindowLines
	}
	if arguments.Containers {
		return service.RunContainerLogs(ctx, actor, arguments.ID)
	}
	if arguments.NodeID != "" || arguments.Container != "" {
		value, err := service.RunContainerLogFiltered(ctx, actor, arguments.ID, arguments.NodeID, arguments.Container, filter)
		if response, ok := value.(ContainerLogResponse); ok && defaultWindow {
			response.Window = runlog.DefaultWindowDescription
			value = response
		}
		return value, err
	}
	value, err := service.RunLogFiltered(ctx, actor, arguments.ID, arguments.Stage, arguments.Step, filter)
	if err != nil {
		return nil, err
	}
	if response, ok := value.(LogResponse); ok && defaultWindow {
		response.Window = runlog.DefaultWindowDescription
		value = response
	}
	return value, nil
}

func (service *API) callSHALogs(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	var arguments struct {
		Repo       string `json:"repo"`
		NodeID     string `json:"node_id"`
		Container  string `json:"container"`
		Containers bool   `json:"containers"`
		Raw        bool   `json:"raw"`
		SHA        string `json:"sha"`
		Step       string `json:"step"`
		Pattern    string `json:"pattern"`
		Context    int    `json:"context"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		Tail       bool   `json:"tail"`
		ByteOffset *int64 `json:"byte_offset"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	if arguments.NodeID != "" || arguments.Container != "" || arguments.Containers || arguments.Raw {
		if !validSHASelector(arguments.SHA) {
			return nil, fmt.Errorf("%w: SHA is required", ErrInvalidInput)
		}
		_, run, err := service.resolveRun(ctx, arguments.Repo, arguments.SHA)
		if err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		delete(fields, "repo")
		delete(fields, "sha")
		fields["id"], err = json.Marshal(run.ID)
		if err != nil {
			return nil, err
		}
		forwarded, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		return service.callRunLogs(ctx, actor, forwarded)
	}
	filter, err := toolLogFilter(arguments.Pattern, arguments.Context, arguments.Offset, arguments.Limit, arguments.Tail)
	if err != nil {
		return nil, err
	}
	if arguments.ByteOffset != nil && *arguments.ByteOffset < 0 {
		return nil, fmt.Errorf("%w: byte_offset must be nonnegative", ErrInvalidInput)
	}
	filter.ByteOffset = arguments.ByteOffset
	return service.logsFor(ctx, arguments.Repo, arguments.SHA, arguments.Step, filter)
}
