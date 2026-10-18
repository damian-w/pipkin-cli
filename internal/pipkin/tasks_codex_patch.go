package pipkin

import (
	"bytes"
	"encoding/json"
)

type codexTaskPatch struct {
	Op    string          `json:"op"`
	Path  []any           `json:"path"`
	Value json.RawMessage `json:"value"`
}

func codexTaskItemPatch(meta *codexTaskMeta, rest []any, patch codexTaskPatch) bool {
	pending := meta.pendingCards > 0 || meta.uncertainCards || meta.pendingInput || meta.pendingPlan
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
				item.Type == "userInputResponse" || item.Type == "planImplementation" {
				return true
			}
		}
		return bytes.Contains(patch.Value, []byte("send_user_message_question_reply"))
	}
	field, _ := rest[1].(string)
	switch field {
	case "questions", "completed", "isCompleted", "requestId":
		return true
	case "id", "type", "status":
		return pending
	case "text", "content", "input":
		return pending || bytes.Contains(patch.Value, []byte("send_user_message_question_reply"))
	}
	return false
}

// Unsupported changes request a fresh projected snapshot. Patches never rebuild
// or retain the conversation body, and tool output does not affect task state.
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
			if json.Unmarshal(patch.Value, &fields) != nil || bytes.Equal(bytes.TrimSpace(fields.ActiveFlags), []byte("null")) {
				return false, errCodexTaskState
			}
			for _, flag := range runtime.ActiveFlags {
				if flag == "" {
					return false, errCodexTaskState
				}
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
			if json.Unmarshal(patch.Value, &flags) != nil || flags == nil {
				return false, errCodexTaskState
			}
			for _, flag := range flags {
				if flag == "" {
					return false, errCodexTaskState
				}
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
		if json.Unmarshal(patch.Value, &requests) != nil || requests == nil {
			return false, errCodexTaskState
		}
		for _, request := range requests {
			if request.Method == "" {
				return false, errCodexTaskState
			}
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
	case "turns":
		if len(patch.Path) <= 2 {
			return true, nil
		}
		if patch.Path[2] == "items" {
			return codexTaskItemPatch(meta, patch.Path[3:], patch), nil
		}
		switch patch.Path[2] {
		case "turnId", "id", "status", "error":
			return true, nil
		}
	case "turnHistory":
		if len(patch.Path) <= 4 || patch.Path[1] != "history" || patch.Path[2] != "entitiesByKey" {
			return true, nil
		}
		if patch.Path[4] == "items" {
			return codexTaskItemPatch(meta, patch.Path[5:], patch), nil
		}
		switch patch.Path[4] {
		case "turnId", "id", "status", "error":
			return true, nil
		}
	}
	return false, nil
}
