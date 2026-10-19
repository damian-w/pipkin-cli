package pipkin

import (
	"bytes"
	"encoding/json"
	"slices"
)

type codexTaskPatch struct {
	Op    string          `json:"op"`
	Path  []any           `json:"path"`
	Value json.RawMessage `json:"value"`
}

func codexTaskItemPatch(meta *codexTaskMeta, rest []any, patch codexTaskPatch) bool {
	pending := meta.pendingCards > 0 || meta.uncertainCards || meta.pendingInput
	if len(rest) == 0 || len(rest) == 1 {
		if pending || patch.Op == "remove" {
			return true
		}
		items := []codexTaskItem{}
		if len(rest) == 0 {
			if json.Unmarshal(patch.Value, &items) != nil {
				return true
			}
		} else {
			var item codexTaskItem
			if json.Unmarshal(patch.Value, &item) != nil {
				return true
			}
			items = append(items, item)
		}
		for _, item := range items {
			if len(item.Questions) > 0 && !bytes.Equal(item.Questions, []byte("null")) ||
				item.Type == "userInputResponse" {
				return true
			}
		}
		return bytes.Contains(patch.Value, []byte("send_user_message_question_reply"))
	}
	field, _ := rest[1].(string)
	switch field {
	case "questions", "completed", "requestId":
		return true
	case "id", "type", "status":
		return pending
	case "text", "content", "input":
		return pending || bytes.Contains(patch.Value, []byte("send_user_message_question_reply"))
	}
	return false
}

// Unsupported patches request a fresh snapshot; conversation text is never kept.
func codexTaskApply(meta *codexTaskMeta, patch codexTaskPatch) (bool, error) {
	if patch.Op != "add" && patch.Op != "replace" && patch.Op != "remove" {
		return false, errCodexTaskState
	}
	for _, part := range patch.Path {
		if part == "__proto__" || part == "constructor" || part == "prototype" {
			return false, errCodexTaskState
		}
	}
	if len(patch.Path) == 0 {
		return true, nil
	}
	root, ok := patch.Path[0].(string)
	if !ok {
		return false, errCodexTaskState
	}
	switch root {
	case "threadRuntimeStatus":
		if patch.Op == "remove" {
			return true, nil
		}
		switch {
		case len(patch.Path) == 1:
			var runtime codexTaskRuntime
			if json.Unmarshal(patch.Value, &runtime) != nil || runtime.Type == "" {
				return false, errCodexTaskState
			}
			var fields struct {
				ActiveFlags json.RawMessage `json:"activeFlags"`
			}
			if json.Unmarshal(patch.Value, &fields) != nil || bytes.Equal(bytes.TrimSpace(fields.ActiveFlags), []byte("null")) ||
				slices.Contains(runtime.ActiveFlags, "") {
				return false, errCodexTaskState
			}
			meta.runtime = runtime
		case len(patch.Path) == 2 && patch.Path[1] == "type":
			var value *string
			if json.Unmarshal(patch.Value, &value) != nil || value == nil || *value == "" {
				return false, errCodexTaskState
			}
			meta.runtime.Type = *value
		case len(patch.Path) == 2 && patch.Path[1] == "activeFlags":
			var flags []string
			if json.Unmarshal(patch.Value, &flags) != nil || flags == nil || slices.Contains(flags, "") {
				return false, errCodexTaskState
			}
			meta.runtime.ActiveFlags = flags
		default:
			return true, nil
		}
	case "requests":
		if len(patch.Path) != 1 || patch.Op == "remove" {
			return true, nil
		}
		var requests []codexTaskRequest
		if json.Unmarshal(patch.Value, &requests) != nil || requests == nil ||
			slices.ContainsFunc(requests, func(r codexTaskRequest) bool { return r.Method == "" }) {
			return false, errCodexTaskState
		}
		meta.humanRequest, meta.otherRequest = codexTaskRequests(requests)
	case "updatedAt":
		if len(patch.Path) != 1 {
			return false, errCodexTaskState
		}
		meta.updated = nil
		var number json.Number
		if patch.Op != "remove" && json.Unmarshal(patch.Value, &number) == nil {
			if value, err := number.Float64(); err == nil {
				if epoch := validEpoch(value / 1000); epoch != 0 {
					meta.updated = &epoch
				}
			}
		}
	case "id", "source", "threadSource", "parentThreadId", "ephemeral", "sideConversation", "resumeState":
		return true, nil
	case "turns", "turnHistory":
		field := 2
		if root == "turnHistory" {
			field = 4
		}
		if len(patch.Path) <= field || field == 4 && (patch.Path[1] != "history" || patch.Path[2] != "entitiesByKey") {
			return true, nil
		}
		switch patch.Path[field] {
		case "items":
			return codexTaskItemPatch(meta, patch.Path[field+1:], patch), nil
		case "turnId", "id", "status", "error":
			return true, nil
		}
	}
	return false, nil
}
