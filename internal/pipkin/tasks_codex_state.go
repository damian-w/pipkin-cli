package pipkin

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

var errCodexTaskState = errors.New("unsupported task state")

const (
	codexTaskMaxHistoryTurns     = 4096
	codexTaskMaxInteractionItems = 65536
	codexTaskMaxQuestionKeys     = 4096
)

type codexTaskRuntime struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags"`
}

type codexTaskRequest struct {
	Method    string `json:"method"`
	Completed bool   `json:"completed"`
	Params    struct {
		Mode      string          `json:"mode"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

type codexTaskItem struct {
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	Questions   json.RawMessage `json:"questions"`
	Content     json.RawMessage `json:"content"`
	Input       json.RawMessage `json:"input"`
	Status      string          `json:"status"`
	Completed   *bool           `json:"completed"`
	IsCompleted *bool           `json:"isCompleted"`
	RequestID   json.RawMessage `json:"requestId"`
}

type codexTaskTurn struct {
	Status string          `json:"status"`
	Items  []codexTaskItem `json:"items"`
}

type codexTaskSnapshot struct {
	ID               string             `json:"id"`
	UpdatedAt        json.Number        `json:"updatedAt"`
	Runtime          codexTaskRuntime   `json:"threadRuntimeStatus"`
	Requests         []codexTaskRequest `json:"requests"`
	Source           json.RawMessage    `json:"source"`
	ThreadSource     string             `json:"threadSource"`
	ParentThreadID   string             `json:"parentThreadId"`
	Ephemeral        bool               `json:"ephemeral"`
	SideConversation bool               `json:"sideConversation"`
	ResumeState      string             `json:"resumeState"`
	Turns            []codexTaskTurn    `json:"turns"`
	TurnHistory      struct {
		Kind    string `json:"kind"`
		History struct {
			Islands []struct {
				Entries []struct {
					Value string `json:"value"`
				} `json:"entries"`
				NewerBoundary struct {
					Status string `json:"status"`
				} `json:"newerBoundary"`
			} `json:"islands"`
			Entities map[string]codexTaskTurn `json:"entitiesByKey"`
		} `json:"history"`
	} `json:"turnHistory"`
}

// The projection holds no conversation text. Scope is 0 for an internal
// thread, 1 for a user thread and 2 when unknown.
type codexTaskMeta struct {
	updated        *int64
	runtime        codexTaskRuntime
	humanRequest   bool
	otherRequest   bool
	pendingCards   int
	uncertainCards bool
	pendingInput   bool
	pendingPlan    bool
	tailKnown      bool
	tailStatus     string
	scope          int
}

func codexTaskSource(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var source string
	if json.Unmarshal(raw, &source) == nil {
		return source
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(raw, &nested) == nil {
		for _, key := range []string{"subagent", "subAgent", "sub_agent"} {
			if _, ok := nested[key]; ok {
				return "subagent"
			}
		}
	}
	return "unknown"
}

func codexTaskThreadSource(value string) bool {
	return value == "" || value == "user" || value == "chatgpt_handoff"
}

func codexTaskScope(s codexTaskSnapshot, catalogRoot bool) int {
	source := codexTaskSource(s.Source)
	if source == "subagent" || (s.ParentThreadID != "" && !s.SideConversation) {
		return 0
	}
	switch s.ThreadSource {
	case "subagent", "chatgpt_hidden", "ambient_suggestions", "ambient_suggestion_safety", "pull_request_fix_automation", "automation", "title", "summary":
		return 0
	}
	if !codexTaskThreadSource(s.ThreadSource) {
		return 2
	}
	if s.Ephemeral && !s.SideConversation {
		return 2
	}
	if source == "vscode" || (source == "" && (catalogRoot || s.SideConversation)) {
		return 1
	}
	return 2
}

func codexTaskTail(s *codexTaskSnapshot) (*codexTaskTurn, bool) {
	if s.TurnHistory.Kind == "canonical" {
		islands := s.TurnHistory.History.Islands
		if len(islands) == 0 {
			return nil, false
		}
		tail := islands[len(islands)-1]
		if tail.NewerBoundary.Status != "exhausted" {
			return nil, false
		}
		if len(tail.Entries) == 0 {
			return nil, true
		}
		turn, ok := s.TurnHistory.History.Entities[tail.Entries[len(tail.Entries)-1].Value]
		return &turn, ok
	}
	if s.TurnHistory.Kind != "" || s.ResumeState == "needs_resume" || s.ResumeState == "resuming" {
		return nil, false
	}
	if len(s.Turns) == 0 {
		return nil, s.Turns != nil
	}
	return &s.Turns[len(s.Turns)-1], true
}

func codexTaskQuestionKey(value string) (string, bool) {
	var identity []json.RawMessage
	if json.Unmarshal([]byte(value), &identity) != nil || len(identity) != 3 {
		return "", false
	}
	var method, id string
	var index int
	if json.Unmarshal(identity[0], &method) != nil || method != "request_user_input_async" ||
		json.Unmarshal(identity[1], &id) != nil || id == "" || len(id) > 256 ||
		json.Unmarshal(identity[2], &index) != nil || index < 0 || index >= 64 {
		return "", false
	}
	return id + "\x00" + strconv.Itoa(index), true
}

func codexTaskAnswers(raw json.RawMessage, answered map[string]bool) error {
	if !bytes.Contains(raw, []byte("send_user_message_question_reply")) {
		return nil
	}
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &content) != nil || len(content) != 1 || content[0].Type != "text" {
		return nil
	}
	text := strings.TrimSpace(content[0].Text)
	const opening = "<send_user_message_question_reply>"
	const closing = "</send_user_message_question_reply>"
	if !strings.HasPrefix(text, opening) || !strings.HasSuffix(text, closing) {
		return nil
	}
	payload := []byte(text[len(opening) : len(text)-len(closing)])
	type reply struct {
		ID       string  `json:"questionItemId"`
		Question *string `json:"question"`
		Answer   *string `json:"answer"`
	}
	var rows []reply
	if json.Unmarshal(payload, &rows) != nil {
		var row reply
		if json.Unmarshal(payload, &row) != nil {
			return nil
		}
		rows = []reply{row}
	}
	for _, row := range rows {
		if row.Question != nil && row.Answer != nil {
			if key, ok := codexTaskQuestionKey(row.ID); ok {
				answered[key] = true
				if len(answered) > codexTaskMaxQuestionKeys {
					return errCodexTaskState
				}
			}
		}
	}
	return nil
}

func codexTaskTailInput(turn *codexTaskTurn) (input, plan bool) {
	if turn == nil {
		return false, false
	}
	for _, item := range turn.Items {
		switch item.Type {
		case "userInputResponse":
			input = input || (item.Completed != nil && !*item.Completed && len(item.RequestID) > 0 && !bytes.Equal(item.RequestID, []byte("null")))
		case "planImplementation":
			plan = plan || (item.IsCompleted != nil && !*item.IsCompleted)
		}
	}
	return input, plan
}

// A running turn before a gap in loaded history leaves its cards uncertain.
func codexTaskQuestionHistory(s *codexTaskSnapshot) ([]codexTaskTurn, int, error) {
	if s.TurnHistory.Kind != "canonical" {
		if s.TurnHistory.Kind != "" {
			return nil, 0, nil
		}
		if len(s.Turns) > codexTaskMaxHistoryTurns {
			return nil, 0, errCodexTaskState
		}
		certainFrom := 0
		if s.ResumeState == "needs_resume" || s.ResumeState == "resuming" {
			certainFrom = len(s.Turns)
		}
		return s.Turns, certainFrom, nil
	}
	islands := s.TurnHistory.History.Islands
	turns, seen := []codexTaskTurn{}, map[string]bool{}
	certainFrom := -1
	for index, island := range islands {
		if index == len(islands)-1 && island.NewerBoundary.Status == "exhausted" {
			certainFrom = len(turns)
		}
		for _, entry := range island.Entries {
			turn, exists := s.TurnHistory.History.Entities[entry.Value]
			if !exists || seen[entry.Value] || len(turns) >= codexTaskMaxHistoryTurns {
				return nil, 0, errCodexTaskState
			}
			seen[entry.Value] = true
			turns = append(turns, turn)
		}
	}
	if certainFrom < 0 {
		certainFrom = len(turns)
	}
	return turns, certainFrom, nil
}

// Like the Codex app, only cards in the latest running turn count, answered
// only from that turn. Skip/Dismiss isn't in this stream, so a skipped card
// still needs attention until the turn ends.
func codexTaskPendingCards(s *codexTaskSnapshot) (cards int, uncertain bool, err error) {
	turns, certainFrom, err := codexTaskQuestionHistory(s)
	if err != nil {
		return 0, false, err
	}
	turnIndex := len(turns) - 1
	for turnIndex >= 0 && turns[turnIndex].Status != "inProgress" {
		turnIndex--
	}
	if turnIndex < 0 {
		return 0, false, nil
	}
	turn := turns[turnIndex]
	if len(turn.Items) > codexTaskMaxInteractionItems {
		return 0, false, errCodexTaskState
	}
	answered := map[string]bool{}
	for _, item := range turn.Items {
		switch item.Type {
		case "userMessage":
			err = codexTaskAnswers(item.Content, answered)
		case "steeringUserMessage":
			if item.Status == "accepted" {
				err = codexTaskAnswers(item.Input, answered)
			}
		}
		if err != nil {
			return 0, false, err
		}
	}
	questions := map[string]bool{}
	for _, item := range turn.Items {
		if item.Type != "agentMessage" || len(item.Questions) == 0 || bytes.Equal(bytes.TrimSpace(item.Questions), []byte("null")) {
			continue
		}
		var rows []json.RawMessage
		if json.Unmarshal(item.Questions, &rows) != nil || len(rows) > 64 || item.ID == "" || len(item.ID) > 256 {
			return 0, false, errCodexTaskState
		}
		for index := range rows {
			key := item.ID + "\x00" + strconv.Itoa(index)
			if questions[key] {
				return 0, false, errCodexTaskState
			}
			questions[key] = true
			if len(questions) > codexTaskMaxQuestionKeys {
				return 0, false, errCodexTaskState
			}
			if !answered[key] {
				if turnIndex >= certainFrom {
					cards++
				} else {
					uncertain = true
				}
			}
		}
	}
	return cards, uncertain, nil
}

func codexTaskRequests(requests []codexTaskRequest) (human, other bool) {
	for _, request := range requests {
		switch request.Method {
		case "account/chatgptAuthTokens/refresh", "attestation/generate", "currentTime/read", "item/plan/requestImplementation":
			continue
		case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput", "item/tool/requestOptionPicker":
			human = true
		case "mcpServer/elicitation/request":
			if request.Completed {
				continue
			}
			switch request.Params.Mode {
			case "form", "url", "openai/form", "openaiForm", "openai/userVerification":
				human = true
			default:
				other = true
			}
		case "item/tool/call":
			var args struct {
				Question  *string           `json:"question"`
				Options   []json.RawMessage `json:"options"`
				Step      string            `json:"step"`
				Questions []json.RawMessage `json:"questions"`
			}
			if json.Unmarshal(request.Params.Arguments, &args) != nil {
				other = true
				continue
			}
			if request.Params.Tool == "request_option_picker" && args.Question != nil && args.Options != nil ||
				request.Params.Tool == "setup_codex_step" && (args.Step == "role" || args.Step == "task") ||
				request.Params.Tool == "request_onboarding_input" && len(args.Questions) >= 1 && len(args.Questions) <= 3 {
				human = true
			} else {
				other = true
			}
		default:
			other = true
		}
	}
	return human, other
}

func codexTaskProject(raw json.RawMessage, id string, catalogRoot bool) (codexTaskMeta, error) {
	var s codexTaskSnapshot
	if json.Unmarshal(raw, &s) != nil || s.ID != id {
		return codexTaskMeta{}, errCodexTaskState
	}
	meta := codexTaskMeta{runtime: s.Runtime, scope: codexTaskScope(s, catalogRoot)}
	if value, err := s.UpdatedAt.Float64(); err == nil {
		if epoch := validEpoch(value / 1000); epoch != 0 {
			meta.updated = &epoch
		}
	}
	meta.humanRequest, meta.otherRequest = codexTaskRequests(s.Requests)
	if s.Requests == nil {
		meta.otherRequest = true
	}
	tail, known := codexTaskTail(&s)
	meta.tailKnown = known
	if known && tail != nil {
		meta.tailStatus = tail.Status
		meta.pendingInput, meta.pendingPlan = codexTaskTailInput(tail)
	}
	var err error
	meta.pendingCards, meta.uncertainCards, err = codexTaskPendingCards(&s)
	if err != nil {
		return codexTaskMeta{}, err
	}
	return meta, nil
}

func (m codexTaskMeta) status() string {
	if m.scope != 1 {
		return "unknown"
	}
	if m.humanRequest || m.pendingCards > 0 || m.pendingInput {
		return "needs_attention"
	}
	for _, flag := range m.runtime.ActiveFlags {
		if m.runtime.Type == "active" && (flag == "waitingOnApproval" || flag == "waitingOnUserInput") {
			return "needs_attention"
		}
	}
	if m.runtime.Type == "systemError" {
		return "blocked"
	}
	if len(m.runtime.ActiveFlags) > 0 || m.otherRequest || m.uncertainCards || m.tailStatus == "failed" || m.runtime.Type == "active" && m.runtime.ActiveFlags == nil {
		return "unknown"
	}
	if m.tailStatus != "" && m.tailStatus != "inProgress" && m.tailStatus != "completed" && m.tailStatus != "interrupted" {
		return "unknown"
	}
	switch m.runtime.Type {
	case "active":
		return "working"
	case "idle", "notLoaded":
		if m.tailKnown {
			return "idle"
		}
	}
	return "unknown"
}
