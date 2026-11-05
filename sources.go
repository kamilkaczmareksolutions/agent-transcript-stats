package stats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type sessionKey struct {
	source Source
	id     string
}

type contribution interface {
	isContribution()
}

type userMessage struct {
	session sessionKey
	at      time.Time
	text    string
}

type assistantMessage struct {
	session sessionKey
	at      time.Time
}

type toolCall struct {
	session sessionKey
	at      time.Time
}

func (userMessage) isContribution()      {}
func (assistantMessage) isContribution() {}
func (toolCall) isContribution()         {}

type sourceSpec struct {
	source       Source
	experimental bool
	match        func(path string) bool
	session      func(path string) (id string, archive bool, err error)
	parse        func(line []byte, fallback time.Time) (out []contribution, malformed bool)
}

var sourceTable []sourceSpec

var (
	codexLiveRe    = regexp.MustCompile(`(^|/)sessions/[0-9]{4}/[0-9]{2}/[0-9]{2}/rollout-[^/]+\.jsonl$`)
	codexArchiveRe = regexp.MustCompile(`(^|/)archived_sessions/rollout-[^/]+\.jsonl$`)
)

func init() {
	sourceTable = []sourceSpec{
		{
			source:  Claude,
			match:   matchClaude,
			session: sessionClaude,
			parse:   parseClaudeLine,
		},
		{
			source:  Codex,
			match:   matchCodex,
			session: sessionCodex,
			parse:   parseCodexLine,
		},
		{
			source:       Cursor,
			experimental: true,
			match:        matchCursor,
			session:      sessionCursor,
			parse:        parseCursorLine,
		},
	}
	for _, spec := range sourceTable {
		if spec.match == nil || spec.session == nil || spec.parse == nil || spec.source == "" {
			panic("stats: incomplete source spec")
		}
	}
}

func pathSlash(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}

func matchClaude(path string) bool {
	p := pathSlash(path)
	if !strings.HasSuffix(p, ".jsonl") {
		return false
	}
	if strings.Contains(p, "/agent-transcripts/") || strings.Contains(p, "/subagents/") {
		return false
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if seg == "projects" && i+1 < len(parts) {
			return true
		}
	}
	return false
}

func sessionClaude(path string) (string, bool, error) {
	p := pathSlash(path)
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if seg == "projects" && i+1 < len(parts) {
			rest := strings.Join(parts[i+1:], "/")
			rest = strings.TrimSuffix(rest, ".jsonl")
			return rest, false, nil
		}
	}
	return filepath.Base(p), false, nil
}

func matchCodex(path string) bool {
	p := pathSlash(path)
	return codexLiveRe.MatchString(p) || codexArchiveRe.MatchString(p)
}

func sessionCodex(path string) (string, bool, error) {
	p := pathSlash(path)
	archive := strings.Contains(p, "/archived_sessions/")
	id, err := readCodexSessionID(path)
	if err != nil {
		return "", archive, err
	}
	if id == "" {
		base := filepath.Base(p)
		id = strings.TrimSuffix(base, ".jsonl")
	}
	return id, archive, nil
}

func readCodexSessionID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	for {
		line, err := readLineLimited(reader, maxLineLen)
		if err == io.EOF {
			return "", nil
		}
		if errors.Is(err, errLineTooLong) {
			continue
		}
		if err != nil {
			return "", err
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var rec map[string]json.RawMessage
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		var typ string
		if raw, ok := rec["type"]; ok {
			_ = json.Unmarshal(raw, &typ)
		}
		if typ != "session_meta" {
			continue
		}
		rawPayload, ok := rec["payload"]
		if !ok {
			continue
		}
		var payload struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(rawPayload, &payload) != nil {
			continue
		}
		if payload.ID != "" {
			return payload.ID, nil
		}
	}
}

func matchCursor(path string) bool {
	p := pathSlash(path)
	return strings.Contains(p, "/agent-transcripts/") && strings.HasSuffix(p, ".jsonl")
}

func sessionCursor(path string) (string, bool, error) {
	p := pathSlash(path)
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		if seg == "agent-transcripts" && i+1 < len(parts) {
			rest := strings.Join(parts[i+1:], "/")
			rest = strings.TrimSuffix(rest, ".jsonl")
			return rest, false, nil
		}
	}
	return filepath.Base(p), false, nil
}

func classify(path string) (sourceSpec, bool) {
	p := pathSlash(path)
	var hits []sourceSpec
	for _, spec := range sourceTable {
		if spec.match(p) {
			hits = append(hits, spec)
		}
	}
	switch len(hits) {
	case 0:
		return sourceSpec{}, false
	case 1:
		return hits[0], true
	default:
		panic("stats: ambiguous path " + p)
	}
}

func parseTimeField(raw json.RawMessage, fallback time.Time) time.Time {
	if len(raw) == 0 {
		return fallback
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || s == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fallback
	}
	return t.UTC()
}

func parseClaudeLine(line []byte, fallback time.Time) ([]contribution, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(line, &top); err != nil {
		return nil, true
	}
	var typ string
	if raw, ok := top["type"]; ok {
		_ = json.Unmarshal(raw, &typ)
	}
	at := parseTimeField(top["timestamp"], fallback)
	if at.IsZero() {
		at = fallback
	}

	switch typ {
	case "system", "summary", "file-history-snapshot":
		return nil, false
	case "attachment":
		raw, ok := top["attachment"]
		if !ok {
			return nil, false
		}
		var att struct {
			Type   string `json:"type"`
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(raw, &att) != nil {
			return nil, false
		}
		if att.Type == "queued_command" && strings.TrimSpace(att.Prompt) != "" {
			return []contribution{userMessage{at: at, text: att.Prompt}}, false
		}
		return nil, false
	case "user", "assistant":
		return parseClaudeMessageRecord(top, typ, at)
	}

	var role string
	if raw, ok := top["role"]; ok {
		_ = json.Unmarshal(raw, &role)
	}
	if role == "user" || role == "assistant" {
		return parseClaudeRoleRecord(top, role, at)
	}

	if raw, ok := top["message"]; ok {
		var msg struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &msg) == nil {
			if msg.Role == "user" {
				return parseClaudeMessageRecord(top, "user", at)
			}
			if msg.Role == "assistant" {
				return parseClaudeMessageRecord(top, "assistant", at)
			}
		}
	}
	return nil, false
}

func parseClaudeRoleRecord(top map[string]json.RawMessage, role string, at time.Time) ([]contribution, bool) {
	text := claudeAuthoredText(top)
	var out []contribution
	switch role {
	case "user":
		if text != "" {
			out = append(out, userMessage{at: at, text: text})
		}
	case "assistant":
		tools := claudeToolCalls(top)
		if text != "" || tools > 0 {
			out = append(out, assistantMessage{at: at})
		}
		for i := 0; i < tools; i++ {
			out = append(out, toolCall{at: at})
		}
	}
	return out, false
}

func parseClaudeMessageRecord(top map[string]json.RawMessage, typ string, at time.Time) ([]contribution, bool) {
	raw, ok := top["message"]
	if !ok {
		return parseClaudeRoleRecord(top, typ, at)
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return nil, false
	}
	if _, ok := msg["role"]; ok {
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		return parseClaudeRoleRecord(top, role, at)
	}
	return parseClaudeRoleRecord(top, typ, at)
}

func claudeAuthoredText(top map[string]json.RawMessage) string {
	if raw, ok := top["content"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	if raw, ok := top["message"]; ok {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) == nil {
			return textFromContentBlocks(msg.Content)
		}
	}
	return ""
}

func textFromContentBlocks(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		var m map[string]json.RawMessage
		if json.Unmarshal(block, &m) != nil {
			continue
		}
		var btype string
		if t, ok := m["type"]; ok {
			_ = json.Unmarshal(t, &btype)
		}
		if btype != "text" {
			continue
		}
		if t, ok := m["text"]; ok {
			var ts string
			if json.Unmarshal(t, &ts) == nil && ts != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(ts)
			}
		}
	}
	return b.String()
}

func claudeToolCalls(top map[string]json.RawMessage) int {
	raw, ok := top["message"]
	if !ok {
		return 0
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return 0
	}
	return countDedupedToolBlocks(msg.Content)
}

func countDedupedToolBlocks(content json.RawMessage) int {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return 0
	}
	seen := map[string]struct{}{}
	count := 0
	for _, block := range blocks {
		var m map[string]json.RawMessage
		if json.Unmarshal(block, &m) != nil {
			continue
		}
		var btype string
		if t, ok := m["type"]; ok {
			_ = json.Unmarshal(t, &btype)
		}
		if btype != "tool_use" && btype != "server_tool_use" {
			continue
		}
		var id string
		if idRaw, ok := m["id"]; ok {
			_ = json.Unmarshal(idRaw, &id)
		}
		if id != "" {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
		}
		count++
	}
	return count
}

func parseCodexLine(line []byte, fallback time.Time) ([]contribution, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, false
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, true
	}
	var typ string
	if raw, ok := rec["type"]; ok {
		_ = json.Unmarshal(raw, &typ)
	}
	at := parseTimeField(rec["timestamp"], fallback)
	if at.IsZero() {
		at = fallback
	}
	switch typ {
	case "session_meta", "event_msg":
		return nil, false
	case "response_item":
		rawPayload, ok := rec["payload"]
		if !ok {
			return nil, false
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal(rawPayload, &payload) != nil {
			return nil, false
		}
		var ptype string
		if t, ok := payload["type"]; ok {
			_ = json.Unmarshal(t, &ptype)
		}
		switch ptype {
		case "message":
			var role string
			if r, ok := payload["role"]; ok {
				_ = json.Unmarshal(r, &role)
			}
			text := codexMessageText(payload["content"], role)
			var out []contribution
			switch role {
			case "user":
				if text != "" {
					out = append(out, userMessage{at: at, text: text})
				}
			case "assistant":
				if text != "" {
					out = append(out, assistantMessage{at: at})
				}
			}
			return out, false
		case "function_call", "custom_tool_call":
			return []contribution{toolCall{at: at}}, false
		}
	}
	return nil, false
}

func codexMessageText(content json.RawMessage, role string) string {
	if len(content) == 0 {
		return ""
	}
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	want := "input_text"
	if role == "assistant" {
		want = "output_text"
	}
	var b strings.Builder
	for _, block := range blocks {
		var m map[string]json.RawMessage
		if json.Unmarshal(block, &m) != nil {
			continue
		}
		var btype string
		if t, ok := m["type"]; ok {
			_ = json.Unmarshal(t, &btype)
		}
		if btype != want {
			continue
		}
		if t, ok := m["text"]; ok {
			var ts string
			if json.Unmarshal(t, &ts) == nil && ts != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(ts)
			}
		}
	}
	return b.String()
}

var cursorTimestampPrefix = regexp.MustCompile(`^<timestamp>[^<]*</timestamp>`)

func parseCursorLine(line []byte, fallback time.Time) ([]contribution, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, false
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, true
	}
	var role string
	if raw, ok := rec["role"]; ok {
		_ = json.Unmarshal(raw, &role)
	}
	at := parseTimeField(rec["timestamp"], time.Time{})
	rawMsg, ok := rec["message"]
	if !ok {
		return nil, false
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(rawMsg, &msg) != nil {
		return nil, false
	}
	switch role {
	case "user":
		text, wrapperTime := cursorUserText(msg.Content)
		if at.IsZero() {
			at = wrapperTime
		}
		if at.IsZero() {
			at = fallback
		}
		if text == "" {
			return nil, false
		}
		return []contribution{userMessage{at: at, text: text}}, false
	case "assistant":
		if at.IsZero() {
			at = fallback
		}
		text, toolCount := cursorAssistantContent(msg.Content)
		var out []contribution
		if text != "" || toolCount > 0 {
			out = append(out, assistantMessage{at: at})
		}
		for i := 0; i < toolCount; i++ {
			out = append(out, toolCall{at: at})
		}
		return out, false
	}
	return nil, false
}

func cursorUserText(content json.RawMessage) (string, time.Time) {
	blocks := cursorBlocks(content)
	var b strings.Builder
	var wrapper time.Time
	for _, text := range blocks.texts {
		stripped, t := stripCursorTimestamp(text)
		if !t.IsZero() && wrapper.IsZero() {
			wrapper = t
		}
		if stripped != "" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(stripped)
		}
	}
	return b.String(), wrapper
}

func stripCursorTimestamp(s string) (string, time.Time) {
	loc := cursorTimestampPrefix.FindStringIndex(s)
	if loc == nil {
		return s, time.Time{}
	}
	inner := s[loc[0]:loc[1]]
	inner = strings.TrimPrefix(inner, "<timestamp>")
	inner = strings.TrimSuffix(inner, "</timestamp>")
	var t time.Time
	if parsed, err := time.Parse(time.RFC3339, inner); err == nil {
		t = parsed.UTC()
	}
	rest := s[loc[1]:]
	return strings.TrimSpace(rest), t
}

type cursorBlockParse struct {
	texts []string
	tools int
}

func cursorAssistantContent(content json.RawMessage) (string, int) {
	b := cursorBlocks(content)
	return strings.Join(b.texts, " "), b.tools
}

func cursorBlocks(content json.RawMessage) cursorBlockParse {
	var blocks []json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return cursorBlockParse{}
	}
	var out cursorBlockParse
	for _, block := range blocks {
		var m map[string]json.RawMessage
		if json.Unmarshal(block, &m) != nil {
			continue
		}
		var btype string
		if t, ok := m["type"]; ok {
			_ = json.Unmarshal(t, &btype)
		}
		switch btype {
		case "text":
			if t, ok := m["text"]; ok {
				var ts string
				if json.Unmarshal(t, &ts) == nil {
					out.texts = append(out.texts, ts)
				}
			}
		case "tool_use":
			out.tools++
		}
	}
	return out
}
