package stats

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

type Source string

const (
	Claude Source = "claude"
	Codex  Source = "codex"
	Cursor Source = "cursor"
)

type Input struct {
	Roots   []string
	Sources []Source
	TopN    int
}

type Report struct {
	Sessions          int            `json:"sessions"`
	UserMessages      int            `json:"user_messages"`
	AssistantMessages int            `json:"assistant_messages"`
	ToolCalls         int            `json:"tool_calls"`
	TopWords          []WordCount    `json:"top_words"`
	Days              []DayActivity  `json:"days"`
	SkippedLines      int            `json:"skipped_lines"`
	SkippedFiles      int            `json:"skipped_files"`
	Sources           []SourceTotals `json:"sources"`
}

type WordCount struct {
	Word  string `json:"word"`
	Count int    `json:"count"`
}

type DayActivity struct {
	Day               string `json:"day"`
	Sessions          int    `json:"sessions"`
	UserMessages      int    `json:"user_messages"`
	AssistantMessages int    `json:"assistant_messages"`
	ToolCalls         int    `json:"tool_calls"`
}

type SourceTotals struct {
	Source       Source `json:"source"`
	Sessions     int    `json:"sessions"`
	Experimental bool   `json:"experimental"`
}

var ErrBadTopN = errors.New("stats: top must be at least 1")
var ErrBadSource = errors.New("stats: unknown source")

type MissingPathError struct {
	Path string
	Err  error
}

func (e *MissingPathError) Error() string {
	return fmt.Sprintf("stats: path %q: %v", e.Path, e.Err)
}

func (e *MissingPathError) Unwrap() error {
	return e.Err
}

const minWordLen = 3
const maxLineLen = 32 << 20

const dayCountNote = "A session counts on each UTC day it has activity. The total counts each session once."

var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "also": {}, "are": {}, "as": {}, "at": {},
	"be": {}, "but": {}, "by": {}, "can": {}, "for": {}, "from": {}, "had": {},
	"has": {}, "have": {}, "i": {}, "in": {}, "is": {}, "it": {}, "its": {},
	"me": {}, "my": {}, "of": {}, "on": {}, "or": {}, "our": {}, "please": {},
	"that": {}, "the": {}, "their": {}, "them": {}, "this": {}, "to": {},
	"was": {}, "we": {}, "with": {}, "you": {}, "your": {},
}

var errLineTooLong = errors.New("stats: line too long")

var (
	reEmail      = regexp.MustCompile(`(?i)[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reAWSKey     = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	reSkKey      = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`)
	reGhp        = regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`)
	reGithubPat  = regexp.MustCompile(`github_pat_[A-Za-z0-9_\-]{20,}`)
	reSlack      = regexp.MustCompile(`xox[baprs]-[A-Za-z0-9_\-]{10,}`)
	reAPIAssign  = regexp.MustCompile(`(?i)(?:api[_\-]?key|apikey|secret|token|password)\s*[:=]\s*[A-Za-z0-9_\-+/=]{8,}`)
	reHexLong    = regexp.MustCompile(`[0-9a-fA-F]{32,}`)
	reBase64Long = regexp.MustCompile(`[A-Za-z0-9+/]{32,}={0,2}`)
)

func Collect(ctx context.Context, in Input) (Report, error) {
	if in.TopN < 1 {
		return Report{}, ErrBadTopN
	}
	if len(in.Sources) > 0 {
		table := map[Source]struct{}{}
		for _, spec := range sourceTable {
			table[spec.source] = struct{}{}
		}
		for _, s := range in.Sources {
			if _, ok := table[s]; !ok {
				return Report{}, ErrBadSource
			}
		}
	}
	for _, root := range in.Roots {
		if _, err := os.Stat(root); err != nil {
			return Report{}, &MissingPathError{Path: root, Err: err}
		}
	}
	if len(in.Roots) == 0 {
		return emptyReport(), nil
	}
	acc := newAcc()
	skippedLines, skippedFiles, err := walk(ctx, in, acc.fold)
	if err != nil {
		return Report{}, err
	}
	acc.skippedLines = skippedLines
	acc.skippedFiles = skippedFiles
	return acc.finish(in.TopN), nil
}

func emptyReport() Report {
	return Report{
		TopWords: []WordCount{},
		Days:     []DayActivity{},
		Sources:  []SourceTotals{},
	}
}

type acc struct {
	sessions     map[sessionKey]struct{}
	daySessions  map[string]map[sessionKey]struct{}
	dayCounts    map[string]*dayDetail
	user         int
	assistant    int
	tools        int
	words        map[string]int
	bySource     map[Source]map[sessionKey]struct{}
	skippedLines int
	skippedFiles int
}

type dayDetail struct {
	user      int
	assistant int
	tools     int
}

func newAcc() *acc {
	return &acc{
		sessions:    map[sessionKey]struct{}{},
		daySessions: map[string]map[sessionKey]struct{}{},
		dayCounts:   map[string]*dayDetail{},
		words:       map[string]int{},
		bySource:    map[Source]map[sessionKey]struct{}{},
	}
}

func (a *acc) fold(c contribution) {
	switch v := c.(type) {
	case userMessage:
		a.user++
		a.dayBump(v.at, func(d *dayDetail) { d.user++ })
		countWords(v.text, a.words)
		a.markSession(v.session, v.at)
	case assistantMessage:
		a.assistant++
		a.dayBump(v.at, func(d *dayDetail) { d.assistant++ })
		a.markSession(v.session, v.at)
	case toolCall:
		a.tools++
		a.dayBump(v.at, func(d *dayDetail) { d.tools++ })
		a.markSession(v.session, v.at)
	default:
		panic(fmt.Sprintf("stats: unknown contribution %T", c))
	}
}

func (a *acc) dayBump(at time.Time, fn func(*dayDetail)) {
	key := at.UTC().Format("2006-01-02")
	d := a.dayCounts[key]
	if d == nil {
		d = &dayDetail{}
		a.dayCounts[key] = d
	}
	fn(d)
}

func (a *acc) markSession(sk sessionKey, at time.Time) {
	a.sessions[sk] = struct{}{}
	if a.bySource[sk.source] == nil {
		a.bySource[sk.source] = map[sessionKey]struct{}{}
	}
	a.bySource[sk.source][sk] = struct{}{}
	day := at.UTC().Format("2006-01-02")
	if a.daySessions[day] == nil {
		a.daySessions[day] = map[sessionKey]struct{}{}
	}
	a.daySessions[day][sk] = struct{}{}
	d := a.dayCounts[day]
	if d == nil {
		d = &dayDetail{}
		a.dayCounts[day] = d
	}
}

func (a *acc) finish(topN int) Report {
	r := emptyReport()
	r.Sessions = len(a.sessions)
	r.UserMessages = a.user
	r.AssistantMessages = a.assistant
	r.ToolCalls = a.tools
	r.SkippedLines = a.skippedLines
	r.SkippedFiles = a.skippedFiles

	var dayOrder []string
	for day := range a.daySessions {
		dayOrder = append(dayOrder, day)
	}
	sort.Strings(dayOrder)
	for _, day := range dayOrder {
		row := DayActivity{Day: day}
		row.Sessions = len(a.daySessions[day])
		if d, ok := a.dayCounts[day]; ok {
			row.UserMessages = d.user
			row.AssistantMessages = d.assistant
			row.ToolCalls = d.tools
		}
		r.Days = append(r.Days, row)
	}

	type wc struct {
		word  string
		count int
	}
	var words []wc
	for w, n := range a.words {
		words = append(words, wc{word: w, count: n})
	}
	sort.Slice(words, func(i, j int) bool {
		if words[i].count != words[j].count {
			return words[i].count > words[j].count
		}
		return words[i].word < words[j].word
	})
	if len(words) > topN {
		words = words[:topN]
	}
	for _, w := range words {
		r.TopWords = append(r.TopWords, WordCount{Word: w.word, Count: w.count})
	}

	for _, spec := range sourceTable {
		set := a.bySource[spec.source]
		if len(set) == 0 {
			continue
		}
		r.Sources = append(r.Sources, SourceTotals{
			Source:       spec.source,
			Sessions:     len(set),
			Experimental: spec.experimental,
		})
	}
	return r
}

type walkFile struct {
	path    string
	spec    sourceSpec
	id      string
	archive bool
	mtime   time.Time
}

func walk(ctx context.Context, in Input, emit func(contribution)) (skippedLines int, skippedFiles int, err error) {
	allowed := allowedSources(in.Sources)
	bySession := map[sessionKey][]walkFile{}
	seenPath := map[string]struct{}{}

	considerPath := func(path string, fi os.FileInfo) error {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if _, ok := seenPath[abs]; ok {
			return nil
		}
		seenPath[abs] = struct{}{}
		spec, ok := classify(abs)
		if !ok {
			return nil
		}
		if allowed != nil {
			if _, ok := allowed[spec.source]; !ok {
				return nil
			}
		}
		id, archive, err := spec.session(abs)
		if err != nil {
			skippedFiles++
			return nil
		}
		sk := sessionKey{source: spec.source, id: id}
		bySession[sk] = append(bySession[sk], walkFile{
			path: abs, spec: spec, id: id, archive: archive,
			mtime: fi.ModTime().UTC(),
		})
		return nil
	}

	indexFile := func(root string) error {
		info, err := os.Stat(root)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			return considerPath(root, info)
		}
		if !info.IsDir() {
			return nil
		}
		return filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() {
				if d.Name() == "subagents" {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return considerPath(path, fi)
		})
	}

	for _, root := range in.Roots {
		if ctx.Err() != nil {
			return skippedLines, skippedFiles, ctx.Err()
		}
		if err := indexFile(root); err != nil {
			return skippedLines, skippedFiles, err
		}
	}

	var kept []walkFile
	for _, files := range bySession {
		chosen := chooseFiles(files)
		kept = append(kept, chosen...)
	}
	sort.Slice(kept, func(i, j int) bool {
		return kept[i].path < kept[j].path
	})

	for _, f := range kept {
		if ctx.Err() != nil {
			return skippedLines, skippedFiles, ctx.Err()
		}
		sk := sessionKey{source: f.spec.source, id: f.id}
		sl, bad := readLines(f.path, f.mtime, f.spec.parse, func(c contribution) {
			c = withSession(c, sk)
			emit(c)
		})
		skippedLines += sl
		if bad {
			skippedFiles++
		}
	}
	return skippedLines, skippedFiles, nil
}

func allowedSources(sources []Source) map[Source]struct{} {
	if len(sources) == 0 {
		return nil
	}
	out := map[Source]struct{}{}
	for _, s := range sources {
		out[s] = struct{}{}
	}
	return out
}

func chooseFiles(files []walkFile) []walkFile {
	var live, arch []walkFile
	for _, f := range files {
		if f.archive {
			arch = append(arch, f)
		} else {
			live = append(live, f)
		}
	}
	if len(live) > 0 {
		return live
	}
	if len(arch) == 0 {
		return nil
	}
	sort.Slice(arch, func(i, j int) bool {
		if arch[i].mtime.Equal(arch[j].mtime) {
			return arch[i].path < arch[j].path
		}
		return arch[i].mtime.After(arch[j].mtime)
	})
	return []walkFile{arch[0]}
}

func withSession(c contribution, sk sessionKey) contribution {
	switch v := c.(type) {
	case userMessage:
		v.session = sk
		return v
	case assistantMessage:
		v.session = sk
		return v
	case toolCall:
		v.session = sk
		return v
	default:
		panic(fmt.Sprintf("stats: unknown contribution %T", c))
	}
}

func readLines(path string, fallback time.Time, parse func([]byte, time.Time) ([]contribution, bool), emit func(contribution)) (skippedLines int, unreadable bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, true
	}
	defer f.Close()
	if fallback.IsZero() {
		if fi, err := f.Stat(); err == nil {
			fallback = fi.ModTime().UTC()
		}
	}
	reader := bufio.NewReader(f)
	for {
		line, err := readLineLimited(reader, maxLineLen)
		if err == io.EOF {
			break
		}
		if errors.Is(err, errLineTooLong) {
			skippedLines++
			continue
		}
		if err != nil {
			return skippedLines, true
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		out, malformed := parse(line, fallback)
		if malformed {
			skippedLines++
			continue
		}
		for _, c := range out {
			emit(c)
		}
	}
	return skippedLines, false
}

func readLineLimited(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			if err == io.EOF {
				if len(buf) == 0 {
					return nil, io.EOF
				}
				return buf, nil
			}
			return buf, err
		}
		if b == '\n' {
			return append(buf, b), nil
		}
		if len(buf) >= limit {
			for {
				c, derr := r.ReadByte()
				if derr != nil || c == '\n' {
					break
				}
			}
			return nil, errLineTooLong
		}
		buf = append(buf, b)
	}
}

func maskSecrets(text string) string {
	replacers := []*regexp.Regexp{
		reEmail, reAWSKey, reSkKey, reGhp, reGithubPat, reSlack, reAPIAssign, reHexLong, reBase64Long,
	}
	for _, re := range replacers {
		text = re.ReplaceAllStringFunc(text, func(m string) string {
			return strings.Repeat(" ", len(m))
		})
	}
	return text
}

func countWords(text string, into map[string]int) {
	masked := maskSecrets(text)
	fields := strings.FieldsFunc(masked, func(r rune) bool {
		if r == '\'' {
			return true
		}
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, f := range fields {
		w := strings.ToLower(f)
		if len([]rune(w)) < minWordLen {
			continue
		}
		if _, stop := stopwords[w]; stop {
			continue
		}
		into[w]++
	}
}

func (r Report) Text() string {
	var b strings.Builder
	writeCountRow := func(label string, n int) {
		fmt.Fprintf(&b, "%-22s%6d\n", label, n)
	}
	writeCountRow("sessions", r.Sessions)
	writeCountRow("user messages", r.UserMessages)
	writeCountRow("assistant messages", r.AssistantMessages)
	writeCountRow("tool calls", r.ToolCalls)
	b.WriteByte('\n')
	fmt.Fprintf(&b, "%-12s %8s %6s %11s %6s\n", "UTC day", "sessions", "user", "assistant", "tools")
	for _, d := range r.Days {
		fmt.Fprintf(&b, "%-12s %8d %6d %11d %6d\n", d.Day, d.Sessions, d.UserMessages, d.AssistantMessages, d.ToolCalls)
	}
	b.WriteByte('\n')
	b.WriteString(dayCountNote)
	b.WriteByte('\n')
	b.WriteByte('\n')
	b.WriteString("top user words\n")
	for _, w := range r.TopWords {
		fmt.Fprintf(&b, "%6d  %s\n", w.Count, w.Word)
	}
	b.WriteByte('\n')
	b.WriteString("sources\n")
	for _, s := range r.Sources {
		if s.Experimental {
			fmt.Fprintf(&b, "%-8s%6d  experimental\n", s.Source, s.Sessions)
		} else {
			fmt.Fprintf(&b, "%-8s%6d\n", s.Source, s.Sessions)
		}
	}
	b.WriteByte('\n')
	writeCountRow("skipped lines", r.SkippedLines)
	writeCountRow("skipped files", r.SkippedFiles)
	return b.String()
}

func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "| metric | count |\n| --- | ---: |\n")
	fmt.Fprintf(&b, "| sessions | %d |\n", r.Sessions)
	fmt.Fprintf(&b, "| user messages | %d |\n", r.UserMessages)
	fmt.Fprintf(&b, "| assistant messages | %d |\n", r.AssistantMessages)
	fmt.Fprintf(&b, "| tool calls | %d |\n\n", r.ToolCalls)
	b.WriteString("| UTC day | sessions | user | assistant | tools |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: |\n")
	for _, d := range r.Days {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d |\n", d.Day, d.Sessions, d.UserMessages, d.AssistantMessages, d.ToolCalls)
	}
	b.WriteByte('\n')
	b.WriteString(dayCountNote)
	b.WriteByte('\n')
	b.WriteByte('\n')
	b.WriteString("### top user words\n\n")
	for _, w := range r.TopWords {
		fmt.Fprintf(&b, "- %s (%d)\n", w.Word, w.Count)
	}
	b.WriteByte('\n')
	b.WriteString("### sources\n\n")
	for _, s := range r.Sources {
		exp := ""
		if s.Experimental {
			exp = " experimental"
		}
		fmt.Fprintf(&b, "- %s: %d%s\n", s.Source, s.Sessions, exp)
	}
	b.WriteByte('\n')
	fmt.Fprintf(&b, "skipped lines: %d\n", r.SkippedLines)
	fmt.Fprintf(&b, "skipped files: %d\n", r.SkippedFiles)
	return b.String()
}

func (r Report) JSON() ([]byte, error) {
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	out = append(out, '\n')
	return out, nil
}
