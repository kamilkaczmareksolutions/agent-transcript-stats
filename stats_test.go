package stats_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kamilkaczmareksolutions/agent-transcript-stats"
)

func TestExamples(t *testing.T) {
	examples := "examples"
	report, err := stats.Collect(context.Background(), stats.Input{
		Roots: []string{examples},
		TopN:  20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Sessions != 3 {
		t.Fatalf("sessions: got %d want 3", report.Sessions)
	}
	if report.UserMessages != 5 {
		t.Fatalf("user messages: got %d want 5", report.UserMessages)
	}
	if report.AssistantMessages != 3 {
		t.Fatalf("assistant messages: got %d want 3", report.AssistantMessages)
	}
	if report.ToolCalls != 4 {
		t.Fatalf("tool calls: got %d want 4", report.ToolCalls)
	}
	if report.SkippedLines != 1 {
		t.Fatalf("skipped lines: got %d want 1", report.SkippedLines)
	}
	if report.SkippedFiles != 0 {
		t.Fatalf("skipped files: got %d want 0", report.SkippedFiles)
	}

	wantDays := map[string]stats.DayActivity{
		"2026-03-01": {Day: "2026-03-01", Sessions: 2, UserMessages: 3, AssistantMessages: 2, ToolCalls: 3},
		"2026-03-02": {Day: "2026-03-02", Sessions: 1, UserMessages: 1, AssistantMessages: 0, ToolCalls: 0},
		"2026-03-03": {Day: "2026-03-03", Sessions: 1, UserMessages: 1, AssistantMessages: 1, ToolCalls: 1},
	}
	if len(report.Days) != len(wantDays) {
		t.Fatalf("days len: got %d want %d", len(report.Days), len(wantDays))
	}
	for _, d := range report.Days {
		want, ok := wantDays[d.Day]
		if !ok {
			t.Fatalf("unexpected day %s", d.Day)
		}
		if d != want {
			t.Fatalf("day %s: got %+v want %+v", d.Day, d, want)
		}
	}

	wantWords := map[string]int{
		"transcript": 2,
		"calls":      1, "count": 1, "demo": 1, "how": 1, "legacy": 1, "line": 1,
		"many": 1, "secret": 1, "session": 1, "show": 1, "stats": 1, "tally": 1,
		"tool": 1, "words": 1,
	}
	if len(report.TopWords) != len(wantWords) {
		t.Fatalf("top words len: got %d want %d", len(report.TopWords), len(wantWords))
	}
	gotWords := map[string]int{}
	for _, w := range report.TopWords {
		gotWords[w.Word] = w.Count
	}
	for word, count := range wantWords {
		if gotWords[word] != count {
			t.Fatalf("word %q: got %d want %d", word, gotWords[word], count)
		}
	}

	if len(report.Sources) != 3 {
		t.Fatalf("sources: got %d want 3", len(report.Sources))
	}
	wantSources := []stats.SourceTotals{
		{Source: stats.Claude, Sessions: 1, Experimental: false},
		{Source: stats.Codex, Sessions: 1, Experimental: false},
		{Source: stats.Cursor, Sessions: 1, Experimental: true},
	}
	for i, want := range wantSources {
		if report.Sources[i] != want {
			t.Fatalf("source[%d]: got %+v want %+v", i, report.Sources[i], want)
		}
	}

	absent := []string{
		"sk-abcdefghijklmnopqrstuvwxyz012345", "ada@example.com", "ada", "example", "com",
		"archiveonlyword", "eventmirrorword", "toolresultword", "subagentword", "systemword", "snapshotword",
	}
	text := report.Text()
	md := report.Markdown()
	jsonBytes, err := report.JSON()
	if err != nil {
		t.Fatal(err)
	}
	jsonStr := string(jsonBytes)
	for _, s := range absent {
		if strings.Contains(text, s) || strings.Contains(md, s) || strings.Contains(jsonStr, s) {
			t.Fatalf("output contains %q", s)
		}
	}
	if strings.Contains(jsonStr, `"the"`) {
		t.Fatal("json contains stopword the")
	}
}

func TestCursorTimestampFieldBeatsWrapper(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "projects", "demo", "agent-transcripts", "conv")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"role":"user","timestamp":"2026-06-02T00:00:00Z","message":{"content":[{"type":"text","text":"<timestamp>2026-06-01T00:00:00Z</timestamp>field wins here"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, "run.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := stats.Collect(context.Background(), stats.Input{Roots: []string{dir}, TopN: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Days) != 1 || report.Days[0].Day != "2026-06-02" {
		t.Fatalf("days: got %+v", report.Days)
	}
	if report.UserMessages != 1 {
		t.Fatalf("user messages: got %d", report.UserMessages)
	}
}

func TestOverlongLineSkipsOnlyThatLine(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "claude", "projects", "-demo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString("{")
	body.WriteString(strings.Repeat("x", (32<<20)+8))
	body.WriteString("\n")
	body.WriteString(`{"type":"user","timestamp":"2026-05-01T00:00:00Z","message":{"role":"user","content":"after the long line"}}` + "\n")
	if err := os.WriteFile(filepath.Join(root, "sess.jsonl"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := stats.Collect(context.Background(), stats.Input{Roots: []string{dir}, TopN: 10})
	if err != nil {
		t.Fatal(err)
	}
	if report.SkippedLines != 1 {
		t.Fatalf("skipped lines: got %d want 1", report.SkippedLines)
	}
	if report.UserMessages != 1 {
		t.Fatalf("user messages: got %d want 1", report.UserMessages)
	}
	if len(report.Days) != 1 || report.Days[0].Day != "2026-05-01" {
		t.Fatalf("days: got %+v", report.Days)
	}
}

func TestCursorMtimeFallback(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "cursor", "projects", "demo", "agent-transcripts", "conv1")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "run.jsonl")
	line := `{"role":"user","message":{"content":[{"type":"text","text":"hello there friend"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, want, want); err != nil {
		t.Fatal(err)
	}
	report, err := stats.Collect(context.Background(), stats.Input{
		Roots: []string{dir},
		TopN:  20,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range report.Days {
		if d.Day == "2026-04-01" {
			found = true
			if d.UserMessages != 1 {
				t.Fatalf("user messages on 2026-04-01: got %d want 1", d.UserMessages)
			}
		}
	}
	if !found {
		t.Fatal("expected day 2026-04-01")
	}
}

func TestMissingRoot(t *testing.T) {
	_, err := stats.Collect(context.Background(), stats.Input{
		Roots: []string{filepath.Join(t.TempDir(), "missing")},
		TopN:  20,
	})
	var missing *stats.MissingPathError
	if !errors.As(err, &missing) {
		t.Fatalf("expected MissingPathError, got %v", err)
	}
}

func TestBadTopN(t *testing.T) {
	_, err := stats.Collect(context.Background(), stats.Input{TopN: 0})
	if !errors.Is(err, stats.ErrBadTopN) {
		t.Fatalf("got %v", err)
	}
}

func TestBadSource(t *testing.T) {
	_, err := stats.Collect(context.Background(), stats.Input{
		Roots:   []string{"examples"},
		Sources: []stats.Source{"nope"},
		TopN:    20,
	})
	if !errors.Is(err, stats.ErrBadSource) {
		t.Fatalf("got %v", err)
	}
}

func TestEmptyRoots(t *testing.T) {
	report, err := stats.Collect(context.Background(), stats.Input{TopN: 20})
	if err != nil {
		t.Fatal(err)
	}
	if report.Sessions != 0 {
		t.Fatalf("expected zero sessions, got %+v", report)
	}
	if len(report.TopWords) != 0 || len(report.Days) != 0 || len(report.Sources) != 0 {
		t.Fatalf("expected empty slices, got %+v", report)
	}
}

func TestCollectTwice(t *testing.T) {
	in := stats.Input{Roots: []string{"examples"}, TopN: 20}
	a, err := stats.Collect(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := stats.Collect(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Sessions != b.Sessions || a.UserMessages != b.UserMessages || a.ToolCalls != b.ToolCalls {
		t.Fatalf("reports differ:\n%+v\n%+v", a, b)
	}
}
