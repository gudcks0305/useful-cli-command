package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	maxFileSize = 1 << 20
	maxLineSize = 64 << 10
	maxFindings = 512
	maxDisplay  = 240
)

var errFileTooLarge = errors.New("file too large")

type options struct {
	json    bool
	file    string
	current bool
}

type finding struct {
	Kind      string `json:"kind"`
	Source    string `json:"source"`
	Line      int    `json:"line,omitempty"`
	Component string `json:"component,omitempty"`
	Detail    string `json:"detail"`
}

type report struct {
	Source   string    `json:"source"`
	Partial  bool      `json:"partial"`
	Findings []finding `json:"findings"`
}

type pathItem struct {
	value       string
	source      string
	line        int
	placeholder bool
}

type fileOps struct {
	readFile     func(string) ([]byte, error)
	lstat        func(string) (os.FileInfo, error)
	stat         func(string) (os.FileInfo, error)
	evalSymlinks func(string) (string, error)
	getenv       func(string) string
	userHomeDir  func() (string, error)
}

var defaultOps = fileOps{
	readFile:     readBoundedFile,
	lstat:        os.Lstat,
	stat:         os.Stat,
	evalSymlinks: filepath.EvalSymlinks,
	getenv:       os.Getenv,
	userHomeDir:  os.UserHomeDir,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithOps(args, stdout, stderr, defaultOps)
}

func runWithOps(args []string, stdout, stderr io.Writer, ops fileOps) int {
	flags := flag.NewFlagSet("pathdoctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts options
	flags.BoolVar(&opts.json, "json", false, "JSON output")
	flags.StringVar(&opts.file, "file", "", "zsh startup file to audit (default ~/.zshrc)")
	flags.BoolVar(&opts.current, "current", false, "also audit current process PATH")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: pathdoctor [--json] [--file FILE] [--current]")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "pathdoctor: unexpected arguments: %q\n", strings.Join(flags.Args(), " "))
		return 2
	}

	home, err := ops.userHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "pathdoctor: home lookup failed: %q\n", bounded(err.Error()))
		return 2
	}
	if opts.file == "" {
		opts.file = filepath.Join(home, ".zshrc")
	} else {
		opts.file = expandHomeFilename(opts.file, home)
	}

	rep := report{Source: opts.file, Findings: make([]finding, 0)}
	data, err := ops.readFile(opts.file)
	if err != nil {
		rep.Partial = true
		kind := "unreadable"
		if errors.Is(err, errFileTooLarge) {
			kind = "oversized-file"
		}
		rep.add(finding{Kind: kind, Source: opts.file, Detail: bounded(err.Error())})
	} else {
		items, parseFindings, partial := parseZshPATH(string(data), opts.file, home)
		rep.Partial = partial
		for _, f := range parseFindings {
			rep.add(f)
		}
		auditItems(&rep, items, ops)
	}
	if opts.current {
		items := currentItems(ops.getenv("PATH"))
		auditItems(&rep, items, ops)
	}

	sort.SliceStable(rep.Findings, func(i, j int) bool {
		a, b := rep.Findings[i], rep.Findings[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Component < b.Component
	})
	if opts.json {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(true)
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "pathdoctor: JSON output failed: %q\n", bounded(err.Error()))
			return 2
		}
	} else {
		printHuman(stdout, rep)
	}
	if rep.Partial {
		return 3
	}
	if len(rep.Findings) != 0 {
		return 1
	}
	return 0
}

func readBoundedFile(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("%w: exceeds %d bytes", errFileTooLarge, maxFileSize)
	}
	return data, nil
}

func (r *report) add(f finding) {
	if len(r.Findings) >= maxFindings {
		r.Partial = true
		return
	}
	f.Source = bounded(f.Source)
	f.Component = bounded(f.Component)
	f.Detail = bounded(f.Detail)
	r.Findings = append(r.Findings, f)
}

func parseZshPATH(input, source, home string) ([]pathItem, []finding, bool) {
	logical, lineFindings, partial := logicalLines(input, source)
	state := []pathItem{{placeholder: true, source: source}}
	findings := append([]finding(nil), lineFindings...)
	for _, ln := range logical {
		statement, ok := stripComment(ln.text)
		if !ok {
			findings = append(findings, dynamicFinding(source, ln.line, "unterminated quote"))
			partial = true
			continue
		}
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if externalMutationStatement(statement) {
			findings = append(findings, externalMutationFinding(source, ln.line))
			partial = true
		}
		name, appendMode, rhs, recognized := assignment(statement)
		if !recognized {
			continue
		}
		var parsed []pathItem
		var issue bool
		if name == "PATH" {
			parsed, issue = parseScalar(rhs, source, ln.line, home)
			if appendMode {
				trimmed := strings.TrimSpace(rhs)
				if strings.HasPrefix(trimmed, ":") && len(parsed) > 0 && parsed[0].value == "" && !parsed[0].placeholder {
					// Scalar PATH+=:/dir appends a separator plus a new entry;
					// the leading colon is not an empty PATH component.
					parsed = parsed[1:]
				} else if trimmed != "" {
					// Without a leading separator scalar += concatenates onto the
					// last inherited component, whose value may be unknown.
					issue = true
				}
			}
		} else {
			parsed, issue = parseArray(rhs, source, ln.line, home)
		}
		if issue {
			findings = append(findings, dynamicFinding(source, ln.line, "dynamic or unsupported expression redacted"))
			partial = true
		}
		if appendMode {
			state = append(state, splicePlaceholders(parsed, state)...)
		} else {
			state = splicePlaceholders(parsed, state)
		}
	}
	return state, findings, partial
}

// externalMutationStatement recognizes commands that can replace or mutate
// PATH outside this parser's static model. It only examines command starts and
// shell-list boundaries; arguments are intentionally never retained.
func externalMutationStatement(statement string) bool {
	for _, segment := range commandSegments(statement) {
		segment = strings.TrimSpace(segment)
		for {
			before := segment
			if strings.HasPrefix(segment, "{") && (len(segment) == 1 || isSpace(segment[1])) {
				segment = strings.TrimSpace(segment[1:])
			} else if strings.HasPrefix(segment, "(") {
				segment = strings.TrimSpace(segment[1:])
			}
			for _, prefix := range []string{"then", "else", "do", "if", "while", "until", "!", "command", "builtin"} {
				if hasCommandPrefix(segment, prefix) {
					segment = strings.TrimSpace(segment[len(prefix):])
					break
				}
			}
			if segment == before {
				break
			}
		}
		if hasCommandPrefix(segment, "source") || hasCommandPrefix(segment, "eval") || hasCommandPrefix(segment, ".") {
			return true
		}
	}
	return false
}

func commandSegments(statement string) []string {
	segments := make([]string, 0, 2)
	start := 0
	var quote byte
	escaped := false
	for i := 0; i < len(statement); i++ {
		c := statement[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote = c
			continue
		}
		if quote == c {
			quote = 0
			continue
		}
		if quote != 0 {
			continue
		}
		separatorLength := 0
		switch c {
		case ';':
			separatorLength = 1
		case '&', '|':
			if i+1 < len(statement) && statement[i+1] == c {
				separatorLength = 2
			}
		}
		if separatorLength == 0 {
			continue
		}
		segments = append(segments, statement[start:i])
		i += separatorLength - 1
		start = i + 1
	}
	segments = append(segments, statement[start:])
	return segments
}

func hasCommandPrefix(statement, command string) bool {
	if !strings.HasPrefix(statement, command) {
		return false
	}
	return len(statement) == len(command) || isSpace(statement[len(command)])
}

// splicePlaceholders models zsh's tied PATH/path value without resolving the
// process environment. Each $PATH/$path occurrence expands to the state that
// existed immediately before the assignment. The initial unknown sentinel is
// retained so inherited entries are never mistaken for dead entries.
func splicePlaceholders(parsed, prior []pathItem) []pathItem {
	size := len(parsed)
	for _, item := range parsed {
		if item.placeholder {
			size += len(prior) - 1
		}
	}
	result := make([]pathItem, 0, size)
	for _, item := range parsed {
		if item.placeholder {
			result = append(result, prior...)
			continue
		}
		result = append(result, item)
	}
	return result
}

type logicalLine struct {
	line int
	text string
}

func logicalLines(input, source string) ([]logicalLine, []finding, bool) {
	raw := strings.Split(input, "\n")
	result := make([]logicalLine, 0, len(raw))
	findings := make([]finding, 0)
	partial := false
	var b strings.Builder
	start := 1
	for i, line := range raw {
		lineNo := i + 1
		if len(line) > maxLineSize {
			findings = append(findings, finding{Kind: "oversized-line", Source: source, Line: lineNo, Detail: fmt.Sprintf("line exceeds %d bytes", maxLineSize)})
			partial = true
			line = line[:maxLineSize]
		}
		if b.Len() == 0 {
			start = lineNo
		}
		continued := hasContinuation(line)
		if continued {
			line = line[:len(line)-1]
		}
		if stripped, ok := stripComment(line); ok {
			line = stripped
		}
		b.WriteString(line)
		if continued {
			continue
		}
		if arrayNeedsMore(b.String()) {
			b.WriteByte('\n')
			continue
		}
		result = append(result, logicalLine{line: start, text: b.String()})
		b.Reset()
	}
	if b.Len() != 0 {
		findings = append(findings, dynamicFinding(source, start, "unterminated continuation"))
		partial = true
	}
	return result, findings, partial
}

func arrayNeedsMore(s string) bool {
	statement := strings.TrimSpace(s)
	name, _, rhs, ok := assignment(statement)
	if !ok || name != "path" || !strings.HasPrefix(strings.TrimSpace(rhs), "(") {
		return false
	}
	depth := 0
	var quote byte
	escaped := false
	for i := 0; i < len(rhs); i++ {
		c := rhs[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote = c
			continue
		}
		if quote == c {
			quote = 0
			continue
		}
		if quote == 0 {
			switch c {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
	}
	return depth > 0
}

func hasContinuation(s string) bool {
	if s == "" || s[len(s)-1] != '\\' {
		return false
	}
	backslashes := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		backslashes++
	}
	if backslashes%2 == 0 {
		return false
	}
	var quote byte
	escaped := false
	for i := 0; i < len(s)-1; i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote = c
		} else if quote == c {
			quote = 0
		}
	}
	return quote != '\''
}

func stripComment(s string) (string, bool) {
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == 0 {
			if c == '\'' || c == '"' {
				quote = c
			} else if c == '#' && (i == 0 || isSpace(s[i-1])) {
				return s[:i], true
			}
		} else if c == quote {
			quote = 0
		}
	}
	return s, quote == 0 && !escaped
}

func assignment(s string) (name string, appendMode bool, rhs string, ok bool) {
	s = strings.TrimSpace(s)
	if remainder, declaration := stripDeclarationPrefix(s); declaration {
		s = remainder
	}
	for _, candidate := range []string{"PATH", "path"} {
		if !strings.HasPrefix(s, candidate) {
			continue
		}
		rest := s[len(candidate):]
		if strings.HasPrefix(rest, "+=") {
			return candidate, true, strings.TrimSpace(rest[2:]), true
		}
		if strings.HasPrefix(rest, "=") {
			return candidate, false, strings.TrimSpace(rest[1:]), true
		}
	}
	return "", false, "", false
}

func stripDeclarationPrefix(statement string) (string, bool) {
	for _, command := range []string{"export", "typeset"} {
		if !hasCommandPrefix(statement, command) {
			continue
		}
		rest := strings.TrimSpace(statement[len(command):])
		for rest != "" && (rest[0] == '-' || rest[0] == '+') {
			end := 0
			for end < len(rest) && !isSpace(rest[end]) {
				end++
			}
			option := rest[:end]
			rest = strings.TrimSpace(rest[end:])
			if option == "--" {
				break
			}
		}
		return rest, true
	}
	return statement, false
}

func parseScalar(rhs, source string, line int, home string) ([]pathItem, bool) {
	parts, issue := splitScalar(rhs)
	items := make([]pathItem, 0, len(parts))
	for _, part := range parts {
		item, unresolved := resolvePart(part.text, part.quoted, part.singleDollar, part.otherDollar, true, source, line, home)
		if unresolved {
			issue = true
			continue
		}
		items = append(items, item)
	}
	return items, issue
}

type shellPart struct {
	text         string
	quoted       bool
	singleDollar bool
	otherDollar  bool
}

func splitScalar(s string) ([]shellPart, bool) {
	parts := make([]shellPart, 0, 4)
	var b strings.Builder
	var quote byte
	escaped := false
	quoted := false
	singleDollar := false
	otherDollar := false
	issue := false
	flush := func() {
		parts = append(parts, shellPart{text: b.String(), quoted: quoted, singleDollar: singleDollar, otherDollar: otherDollar})
		b.Reset()
		quoted = false
		singleDollar = false
		otherDollar = false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			b.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote = c
			quoted = true
			continue
		}
		if quote == c {
			quote = 0
			continue
		}
		// PATH is colon-separated after quote removal too. Quoting the scalar
		// assignment does not make a colon part of a directory name.
		if c == ':' {
			flush()
			continue
		}
		if quote == 0 && (isSpace(c) || c == ';' || c == '&' || c == '|') {
			issue = true
		}
		if c == '$' {
			if quote == '\'' {
				singleDollar = true
			} else {
				otherDollar = true
			}
		}
		b.WriteByte(c)
	}
	if escaped || quote != 0 {
		issue = true
	}
	flush()
	return parts, issue
}

func parseArray(rhs, source string, line int, home string) ([]pathItem, bool) {
	rhs = strings.TrimSpace(rhs)
	if len(rhs) < 2 || rhs[0] != '(' || rhs[len(rhs)-1] != ')' {
		return nil, true
	}
	words, issue := splitWords(rhs[1 : len(rhs)-1])
	items := make([]pathItem, 0, len(words))
	for _, word := range words {
		item, unresolved := resolvePart(word.text, word.quoted, word.singleDollar, word.otherDollar, false, source, line, home)
		if unresolved {
			issue = true
			continue
		}
		items = append(items, item)
	}
	return items, issue
}

func splitWords(s string) ([]shellPart, bool) {
	words := make([]shellPart, 0, 4)
	var b strings.Builder
	var quote byte
	escaped := false
	quoted := false
	singleDollar := false
	otherDollar := false
	active := false
	issue := false
	flush := func() {
		if active {
			words = append(words, shellPart{text: b.String(), quoted: quoted, singleDollar: singleDollar, otherDollar: otherDollar})
		}
		b.Reset()
		quoted, active = false, false
		singleDollar, otherDollar = false, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			b.WriteByte(c)
			escaped, active = false, true
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			active = true
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote, quoted, active = c, true, true
			continue
		}
		if quote == c {
			quote = 0
			continue
		}
		if quote == 0 && isSpace(c) {
			flush()
			continue
		}
		if quote == 0 && (c == ';' || c == '\n') {
			flush()
			continue
		}
		if c == '$' {
			if quote == '\'' {
				singleDollar = true
			} else {
				otherDollar = true
			}
		}
		b.WriteByte(c)
		active = true
	}
	if escaped || quote != 0 {
		issue = true
	}
	flush()
	return words, issue
}

func resolvePart(value string, quoted, singleDollar, otherDollar, scalar bool, source string, line int, home string) (pathItem, bool) {
	item := pathItem{source: source, line: line}
	if !singleDollar && ((scalar && (value == "$PATH" || value == "${PATH}")) ||
		(!scalar && (value == "$path" || value == "${path}" || value == "${path[@]}" || value == "$PATH" || value == "${PATH}"))) {
		item.placeholder = true
		return item, false
	}
	if singleDollar {
		if otherDollar {
			return item, true
		}
		// Parameter expansion is disabled inside single quotes. Keep the bytes
		// literal; audit will correctly classify this as a relative component.
		item.value = value
		return item, false
	}
	value = expandLiteralHome(value, home)
	if containsDynamic(value, quoted) {
		return item, true
	}
	if !quoted && value == "~" {
		value = home
	} else if !quoted && strings.HasPrefix(value, "~/") {
		value = filepath.Join(home, value[2:])
	}
	item.value = value
	return item, false
}

func containsDynamic(s string, _ bool) bool {
	if strings.Contains(s, "$(") || strings.ContainsRune(s, '`') || strings.Contains(s, "${") || strings.ContainsRune(s, '$') {
		return true
	}
	if strings.ContainsAny(s, "*?[") {
		return true
	}
	return false
}

func expandLiteralHome(value, home string) string {
	value = strings.ReplaceAll(value, "${HOME}", home)
	var b strings.Builder
	for i := 0; i < len(value); {
		if strings.HasPrefix(value[i:], "$HOME") {
			next := i + len("$HOME")
			if next == len(value) || !isNameByte(value[next]) {
				b.WriteString(home)
				i = next
				continue
			}
		}
		b.WriteByte(value[i])
		i++
	}
	return b.String()
}

func isNameByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func currentItems(path string) []pathItem {
	parts := strings.Split(path, string(os.PathListSeparator))
	items := make([]pathItem, 0, len(parts))
	for _, part := range parts {
		items = append(items, pathItem{value: part, source: "current PATH"})
	}
	return items
}

func auditItems(rep *report, items []pathItem, ops fileOps) {
	seen := make(map[string]pathItem)
	for _, item := range items {
		if item.placeholder {
			continue
		}
		if item.value == "" {
			rep.add(finding{Kind: "empty-component", Source: item.source, Line: item.line, Component: "", Detail: "empty PATH component exposes current directory"})
			continue
		}
		if !filepath.IsAbs(item.value) {
			rep.add(finding{Kind: "relative", Source: item.source, Line: item.line, Component: item.value, Detail: "relative PATH component depends on current directory"})
			key := "relative:" + filepath.Clean(item.value)
			if first, ok := seen[key]; ok {
				rep.add(duplicateFinding(item, first))
			} else {
				seen[key] = item
			}
			continue
		}

		info, err := ops.lstat(item.value)
		if err != nil {
			kind := "missing"
			detail := "PATH component does not exist"
			if !errors.Is(err, os.ErrNotExist) {
				kind, detail = "stat-error", "PATH component metadata unavailable"
				rep.Partial = true
			}
			rep.add(finding{Kind: kind, Source: item.source, Line: item.line, Component: item.value, Detail: detail})
			key := filepath.Clean(item.value)
			if first, ok := seen[key]; ok {
				rep.add(duplicateFinding(item, first))
			} else {
				seen[key] = item
			}
			continue
		}

		canonical := filepath.Clean(item.value)
		resolved, resolveErr := ops.evalSymlinks(item.value)
		if resolveErr == nil {
			canonical = filepath.Clean(resolved)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if resolveErr != nil {
				rep.add(finding{Kind: "broken-symlink", Source: item.source, Line: item.line, Component: item.value, Detail: "PATH component is a broken symlink"})
				continue
			}
			info, err = ops.stat(item.value)
			if err != nil {
				rep.Partial = true
				rep.add(finding{Kind: "stat-error", Source: item.source, Line: item.line, Component: item.value, Detail: "symlink target metadata unavailable"})
				continue
			}
		}
		if !info.IsDir() {
			rep.add(finding{Kind: "not-directory", Source: item.source, Line: item.line, Component: item.value, Detail: "PATH component is not a directory"})
		} else if info.Mode().Perm()&0o002 != 0 {
			rep.add(finding{Kind: "world-writable", Source: item.source, Line: item.line, Component: item.value, Detail: "PATH directory is world-writable"})
		}
		if first, ok := seen[canonical]; ok {
			rep.add(duplicateFinding(item, first))
		} else {
			seen[canonical] = item
		}
	}
}

func duplicateFinding(item, first pathItem) finding {
	where := first.source
	if first.line > 0 {
		where += ":" + strconv.Itoa(first.line)
	}
	return finding{Kind: "duplicate", Source: item.source, Line: item.line, Component: item.value, Detail: "duplicates component first seen at " + bounded(where)}
}

func dynamicFinding(source string, line int, detail string) finding {
	return finding{Kind: "dynamic-unresolved", Source: source, Line: line, Component: "[redacted]", Detail: detail}
}

func externalMutationFinding(source string, line int) finding {
	return finding{
		Kind:      "external-mutation-unresolved",
		Source:    source,
		Line:      line,
		Component: "[redacted]",
		Detail:    "source or eval may mutate PATH; expression redacted",
	}
}

func printHuman(w io.Writer, rep report) {
	status := "complete"
	if rep.Partial {
		status = "partial"
	}
	fmt.Fprintf(w, "pathdoctor source=%q status=%s findings=%d\n", bounded(rep.Source), status, len(rep.Findings))
	for _, f := range rep.Findings {
		location := f.Source
		if f.Line > 0 {
			location += ":" + strconv.Itoa(f.Line)
		}
		fmt.Fprintf(w, "- %s %q", f.Kind, bounded(location))
		if f.Component != "" || f.Kind == "empty-component" {
			fmt.Fprintf(w, " component=%q", bounded(f.Component))
		}
		fmt.Fprintf(w, ": %q\n", bounded(f.Detail))
	}
}

func expandHomeFilename(name, home string) string {
	if name == "~" {
		return home
	}
	if strings.HasPrefix(name, "~/") {
		return filepath.Join(home, name[2:])
	}
	return name
}

func bounded(s string) string {
	if len(s) <= maxDisplay {
		return s
	}
	return s[:maxDisplay] + "..."
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
