// Package tomledit performs comment-preserving, line-level edits on the
// top-level tables of a TOML file.
//
// It exists because hubbub's operational files are hand-edited and their
// comments are load-bearing documentation — the whole reason the project took
// a TOML dependency at all. A decode/re-encode round trip through
// BurntSushi's encoder drops every comment and sorts every key, so one
// dashboard click would turn an operator's annotated file into an
// unrecognisable diff. Edits here are splices into the original bytes: the
// regions nobody touched come out the other side byte-identical.
//
// The locator below is deliberately *not* trusted. Every span it produces is
// checked against the real TOML parser before a caller may write it (Verify),
// so this is a locator with an independent oracle rather than a second TOML
// implementation that has to be correct on its own. That distinction is what
// makes the pragmatic shortcuts in the scanner safe: when it is wrong, the
// parser says so and the edit is refused.
package tomledit

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// ErrNotFound is returned when no top-level table with the given id exists.
var ErrNotFound = errors.New("no such table")

// ErrUnsupported is returned for a table this package refuses to edit —
// dotted-key entries with no header, and tables that own sub-tables. Both are
// legal TOML that a line-level splice cannot handle without risking a silent
// half-edit, so they are pushed back to the operator rather than guessed at.
var ErrUnsupported = errors.New("table shape not supported by the editor")

// bom is the UTF-8 byte-order mark, written as an escape because a literal
// one in this file would be a BOM in the middle of a Go source file.
var bom = []byte("\ufeff")

type lineKind int

const (
	kindBlank lineKind = iota
	kindComment
	kindHeader
	kindOther
)

type line struct {
	start, end int // byte offsets into src; end is exclusive and past the newline
	kind       lineKind
	// structural records that the line *began* outside any string and at
	// bracket depth zero. A line inside a multi-line string or a multi-line
	// array is a value continuation, never a header, however much it looks
	// like one — `[ -z "$1" ]` in a shell snippet is the case this exists for.
	structural bool
	id         string // kindHeader: the decoded table id
	simple     bool   // kindHeader: a plain [x], not [[x]] and not [a.b]
}

type mlKind int

const (
	mlNone mlKind = iota
	mlBasic
	mlLiteral
)

// scan walks the source once, classifying each physical line and carrying
// string/bracket state across line boundaries.
func scan(src []byte) []line {
	var lines []line
	ml := mlNone
	depth := 0
	for pos := 0; pos < len(src); {
		end := len(src)
		if nl := bytes.IndexByte(src[pos:], '\n'); nl >= 0 {
			end = pos + nl + 1
		}
		raw := src[pos:end]
		l := line{start: pos, end: end, structural: ml == mlNone && depth == 0}
		if l.structural {
			l.kind, l.id, l.simple = classify(raw)
		} else {
			l.kind = kindOther
		}
		ml, depth = advance(raw, ml, depth)
		lines = append(lines, l)
		pos = end
	}
	return lines
}

// advance updates multi-line-string and bracket state across one line.
func advance(raw []byte, ml mlKind, depth int) (mlKind, int) {
	for i := 0; i < len(raw); {
		switch ml {
		case mlBasic:
			switch {
			case raw[i] == '\\':
				i += 2 // an escaped delimiter must not close the string
			case at(raw, i, `"""`):
				ml = mlNone
				i += 3
			default:
				i++
			}
		case mlLiteral:
			if at(raw, i, "'''") {
				ml = mlNone
				i += 3
				continue
			}
			i++
		default:
			switch c := raw[i]; c {
			case '#':
				return ml, depth // rest of the line is a comment
			case '"':
				if at(raw, i, `"""`) {
					ml = mlBasic
					i += 3
					continue
				}
				i = skipBasic(raw, i)
			case '\'':
				if at(raw, i, "'''") {
					ml = mlLiteral
					i += 3
					continue
				}
				i = skipLiteral(raw, i)
			case '[', '{':
				depth++
				i++
			case ']', '}':
				if depth > 0 {
					depth--
				}
				i++
			default:
				i++
			}
		}
	}
	return ml, depth
}

func at(b []byte, i int, s string) bool {
	return i+len(s) <= len(b) && string(b[i:i+len(s)]) == s
}

// skipBasic consumes a single-line basic string starting at the quote in b[i].
// An unterminated string runs to end of line; the parser rejects that anyway,
// and Verify is what turns it into a refusal rather than a bad splice.
func skipBasic(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		case '\n':
			return j
		}
	}
	return len(b)
}

// skipLiteral consumes a single-line literal string. Literal strings have no
// escape sequences at all, so the first closing quote wins.
func skipLiteral(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		if b[j] == '\'' || b[j] == '\n' {
			if b[j] == '\n' {
				return j
			}
			return j + 1
		}
	}
	return len(b)
}

func classify(raw []byte) (lineKind, string, bool) {
	// A byte-order mark is only ever legal at offset 0, where it sits on the
	// same line as the first header and would otherwise hide it.
	t := bytes.Trim(bytes.TrimPrefix(raw, bom), " \t\r\n")
	switch {
	case len(t) == 0:
		return kindBlank, "", false
	case t[0] == '#':
		return kindComment, "", false
	case t[0] == '[':
		if id, simple, ok := parseHeader(t); ok {
			return kindHeader, id, simple
		}
	}
	return kindOther, "", false
}

// parseHeader reads a table header, honouring quoted keys. It reports simple
// only for a single-part [x]: [[x]] is an array of tables and [a.b] is a
// sub-table, and conflating either with [x] would let an edit land on the
// wrong thing.
func parseHeader(t []byte) (id string, simple bool, ok bool) {
	arrayTable := at(t, 0, "[[")
	i := 1
	if arrayTable {
		i = 2
	}
	var parts []string
	var cur strings.Builder
	quoted := false
	for i < len(t) {
		switch c := t[i]; c {
		case ']':
			parts = append(parts, cur.String())
			// Anything after the closing bracket other than whitespace or a
			// comment means this is not a header line we understand.
			rest := bytes.TrimLeft(t[i+1:], "] \t")
			if len(rest) > 0 && rest[0] != '#' {
				return "", false, false
			}
			id = strings.Join(parts, ".")
			if id == "" {
				return "", false, false
			}
			return id, len(parts) == 1 && !arrayTable, true
		case '.':
			parts = append(parts, cur.String())
			cur.Reset()
			i++
		case '"':
			end := skipBasic(t, i)
			s, err := unquoteBasic(string(t[i:end]))
			if err != nil {
				return "", false, false
			}
			cur.WriteString(s)
			quoted = true
			i = end
		case '\'':
			end := skipLiteral(t, i)
			if end <= i+1 {
				return "", false, false
			}
			cur.WriteString(string(t[i+1 : end-1]))
			quoted = true
			i = end
		case ' ', '\t':
			i++
		default:
			if quoted {
				return "", false, false
			}
			cur.WriteByte(c)
			i++
		}
	}
	return "", false, false
}

// unquoteBasic leans on the TOML parser rather than hand-rolling escape
// handling for a case that is already rare.
func unquoteBasic(s string) (string, error) {
	var v struct {
		K string `toml:"k"`
	}
	if _, err := toml.Decode("k = "+s, &v); err != nil {
		return "", err
	}
	return v.K, nil
}

// IDs lists the simple top-level table ids in source order.
func IDs(src []byte) []string {
	var ids []string
	for _, l := range scan(src) {
		if l.structural && l.kind == kindHeader && l.simple {
			ids = append(ids, l.id)
		}
	}
	return ids
}

// Span reports the byte range of a table's whole block: its leading comment
// run, its header, and its body.
//
// Two boundary rules carry the weight here, and both were written against the
// shipped example files:
//
//   - The block ENDS before any trailing run of blank lines and comments.
//     example/channels.toml is [ntfy] followed by forty lines of commented-out
//     [email] documentation running to EOF. A naive "spans to the next table
//     or EOF" rule deletes all of it on the first edit, produces valid TOML,
//     and passes every validation there is — silent, total documentation loss.
//   - A leading comment run that starts at line 1 of the file is file-level
//     preamble, never the block's. example/keys.toml is one blank line away
//     from RemoveBlock("dev") eating its own how-to.
//
// Comments *between* values stay inside the block: only the trailing run is
// excluded. Anything excluded merely stays in the file, so the failure
// direction is "a comment was left behind", never "a comment was deleted".
func Span(src []byte, id string) (start, end int, err error) {
	lines := scan(src)
	h := -1
	for i, l := range lines {
		if !l.structural || l.kind != kindHeader {
			continue
		}
		if l.id == id {
			if !l.simple {
				return 0, 0, fmt.Errorf("%w: %q is an array of tables or a sub-table", ErrUnsupported, id)
			}
			h = i
			break
		}
	}
	if h < 0 {
		return 0, 0, fmt.Errorf("%w: %q", ErrNotFound, id)
	}

	// Refuse a table that owns sub-tables. Splicing [x] while [x.y] lives
	// further down would edit half an entity and leave the rest orphaned.
	for _, l := range lines[h+1:] {
		if l.structural && l.kind == kindHeader && strings.HasPrefix(l.id, id+".") {
			return 0, 0, fmt.Errorf("%w: %q owns the sub-table %q", ErrUnsupported, id, l.id)
		}
	}

	first := h
	for first > 0 && lines[first-1].structural && lines[first-1].kind == kindComment {
		first--
	}
	if first == 0 {
		first = h // the run reaches the top of the file: preamble, not ours
	}

	next := len(lines)
	for i := h + 1; i < len(lines); i++ {
		if lines[i].structural && lines[i].kind == kindHeader {
			next = i
			break
		}
	}

	last := trimTrailing(lines, h, next)
	return lines[first].start, lines[last].end, nil
}

// BodySpan reports the byte range of a table's body — everything after the
// header line, up to the same end as Span.
//
// The dashboard edits this, not Span: if the header were in the textarea an
// operator could rename [ntfy] to [ntfy2] by typing, which reads to the outbox
// engine as "channel removed from config" and makes it settle the backlog and
// delete the spool directory. Queued notifications gone, no warning. Keeping
// the header out of reach removes that hazard entirely.
func BodySpan(src []byte, id string) (start, end int, err error) {
	lines := scan(src)
	blockStart, blockEnd, err := Span(src, id)
	if err != nil {
		return 0, 0, err
	}
	for _, l := range lines {
		if l.structural && l.kind == kindHeader && l.id == id && l.start >= blockStart {
			return l.end, max(l.end, blockEnd), nil
		}
	}
	return 0, 0, fmt.Errorf("%w: %q", ErrNotFound, id)
}

// trimTrailing walks back from the line before next, dropping trailing blank
// lines and any comment run that a blank line separates from the body. It
// loops because a file can end with several such runs — channels.toml ends
// with two.
func trimTrailing(lines []line, h, next int) int {
	m := next - 1
	isBlank := func(i int) bool { return lines[i].structural && lines[i].kind == kindBlank }
	isComment := func(i int) bool { return lines[i].structural && lines[i].kind == kindComment }
	for {
		for m > h && isBlank(m) {
			m--
		}
		if m <= h || !isComment(m) {
			break
		}
		j := m
		for j > h && isComment(j) {
			j--
		}
		if !isBlank(j) {
			break // the run is attached to the body, not trailing
		}
		m = j
	}
	if m < h {
		m = h
	}
	return m
}

// Block returns a table's whole block, comments included.
func Block(src []byte, id string) ([]byte, error) {
	start, end, err := Span(src, id)
	if err != nil {
		return nil, err
	}
	return src[start:end], nil
}

// Body returns a table's body — the part the dashboard puts in a textarea.
func Body(src []byte, id string) ([]byte, error) {
	start, end, err := BodySpan(src, id)
	if err != nil {
		return nil, err
	}
	return src[start:end], nil
}

// ReplaceBody swaps a table's body, leaving its header, its leading comments
// and every other byte of the file untouched.
func ReplaceBody(src []byte, id string, body []byte) ([]byte, error) {
	start, end, err := BodySpan(src, id)
	if err != nil {
		return nil, err
	}
	body = normalise(body, dominantEOL(src))
	return splice(src, start, end, body), nil
}

// ReplaceBlock swaps a table's whole block.
func ReplaceBlock(src []byte, id string, block []byte) ([]byte, error) {
	start, end, err := Span(src, id)
	if err != nil {
		return nil, err
	}
	block = normalise(block, dominantEOL(src))
	return splice(src, start, end, block), nil
}

// RemoveBlock deletes a table's block, along with the blank line that
// separated it from what follows so removals don't accumulate gaps.
func RemoveBlock(src []byte, id string) ([]byte, error) {
	start, end, err := Span(src, id)
	if err != nil {
		return nil, err
	}
	lines := scan(src)
	for _, l := range lines {
		if l.start == end && l.structural && l.kind == kindBlank {
			end = l.end
			break
		}
	}
	out := splice(src, start, end, nil)
	// Removing the last block can leave the file ending in blank lines.
	if start >= len(out) {
		out = append(bytes.TrimRight(out, " \t\r\n"), []byte(dominantEOL(src))...)
	}
	return out, nil
}

// AppendBlock adds a new table at the end of the file, separated by one blank
// line. A file that does not end in a newline gets one first — otherwise the
// new header would be glued onto the last value line.
func AppendBlock(src []byte, block []byte) []byte {
	eol := dominantEOL(src)
	block = normalise(block, eol)
	if !bytes.HasSuffix(block, []byte(eol)) {
		block = append(block, []byte(eol)...)
	}
	if len(bytes.TrimSpace(src)) == 0 {
		return block
	}
	out := bytes.TrimRight(src, " \t\r\n")
	out = append(out, []byte(eol+eol)...)
	return append(out, block...)
}

// SetKeyInBlock sets one key inside a table, preserving the position of every
// other key and every in-block comment. A commented-out `# port = 587` is not
// the key: only structural lines count.
func SetKeyInBlock(src []byte, id, key, value string) ([]byte, error) {
	start, end, err := BodySpan(src, id)
	if err != nil {
		return nil, err
	}
	eol := dominantEOL(src)
	body := src[start:end]
	assign := key + " = " + value

	lines := scan(body)
	for i, l := range lines {
		if !l.structural || l.kind != kindOther {
			continue
		}
		name, ok := keyOf(body[l.start:l.end])
		if !ok || name != key {
			continue
		}
		// The whole value, not just its first line: a hand-written
		// `key = [` array spread over several lines would otherwise keep its
		// continuation lines and its closing bracket, and the result would not
		// parse — a rotate or revoke on such a caller failed with a TOML error
		// nobody could act on.
		valueEnd := logicalEnd(lines, i)
		// Keep whatever terminator the line already had.
		repl := []byte(assign)
		if bytes.HasSuffix(body[l.start:valueEnd], []byte("\n")) {
			repl = append(repl, []byte(eol)...)
		}
		return splice(src, start+l.start, start+valueEnd, repl), nil
	}

	// Absent: insert at the top of the body, where a reader looks first.
	return splice(src, start, start, []byte(assign+eol)), nil
}

// KeyLine is one `key = value` assignment found in a block body.
type KeyLine struct {
	Name  string
	Value string // the raw TOML value text, trailing comment included
	Start int    // byte offset of the assignment within the body
	End   int    // byte offset just past the assignment's last newline
}

// logicalEnd extends a structural line's span over the continuation lines of a
// value that does not fit on one line — a multi-line string, or an array with
// its elements one per line. Those lines are not structural, and the assignment
// they belong to is not finished until they are done.
//
// Spans have to cover the whole value or a caller that rewrites one will leave
// the tail of the old value behind. For the dashboard's masking that tail is a
// credential it just claimed to have hidden.
func logicalEnd(lines []line, i int) int {
	end := lines[i].end
	for j := i + 1; j < len(lines) && !lines[j].structural; j++ {
		end = lines[j].end
	}
	return end
}

// KeyLines lists the structural assignments in a block body.
//
// Structural is the operative word: a commented-out `# password = "..."` is
// not an assignment, and neither is a line inside a multi-line string that
// happens to contain an equals sign. The dashboard uses this to mask
// credentials before putting a channel's settings in a browser, so a missed
// line is a leaked password and a false positive is a corrupted script body.
func KeyLines(body []byte) []KeyLine {
	var out []KeyLine
	lines := scan(body)
	for i, l := range lines {
		if !l.structural || l.kind != kindOther {
			continue
		}
		name, ok := keyOf(body[l.start:l.end])
		if !ok {
			continue
		}
		// The span covers the whole value, however many lines it takes — see
		// logicalEnd. keyOf already proved the first '=' in it is the assignment.
		end := logicalEnd(lines, i)
		_, rhs, _ := bytes.Cut(body[l.start:end], []byte("="))
		out = append(out, KeyLine{
			Name:  name,
			Value: string(bytes.Trim(rhs, " \t\r\n")),
			Start: l.start,
			End:   end,
		})
	}
	return out
}

// keyOf reads the bare key from a `key = value` line. Quoted keys are left
// alone: the dashboard only ever sets keys it names itself.
func keyOf(raw []byte) (string, bool) {
	lhs, _, found := bytes.Cut(raw, []byte("="))
	if !found {
		return "", false
	}
	k := string(bytes.Trim(lhs, " \t"))
	if k == "" || strings.ContainsAny(k, "\"'.[]#") {
		return "", false
	}
	return k, true
}

// MaskStrings rewrites the string values in one physical line of TOML.
//
// The dashboard has to put config text in front of an operator — a settings
// body, a confirmation diff — without putting a live credential there too, and
// it must not reformat what it shows on the way. Collapsing an array to a
// single mask loses the difference between a two-key rotation and a one-key
// revoke, and dropping a trailing comment defeats the point of a diff that
// exists to catch comment loss. So substitution is value-for-value, and every
// other byte comes back unchanged.
//
// mask receives the line's bare key name and the raw text inside each string,
// and returns what to show in its place; handing back the text unchanged leaves
// it alone. The name is empty when the line cannot be read as an assignment —
// most importantly a continuation line of a multi-line array, where the strings
// are values but the field they belong to is on an earlier line. Those are
// still offered to mask rather than skipped, because whether an unattributable
// value is safe to show is the caller's policy: keys.toml is nothing but
// credentials, channels.toml mostly is not.
//
// Table headers and comment lines hold no values and come back untouched, which
// keeps this consistent with KeyLines — a commented-out assignment is not a
// live setting.
func MaskStrings(line []byte, mask func(name, raw string) string) []byte {
	if kind, _, _ := classify(line); kind != kindOther {
		return line
	}
	// Skip the left-hand side only when this really is an assignment. An '='
	// inside a continuation line's string is not an assignment operator, and
	// starting the scan past it would land mid-string and mistake the closing
	// quote for an opening one.
	i := 0
	name, isAssign := keyOf(line)
	if isAssign {
		i = bytes.IndexByte(line, '=') + 1
	}

	out := make([]byte, 0, len(line))
	out = append(out, line[:i]...)
	for i < len(line) {
		switch c := line[i]; {
		case c == '#':
			// A trailing comment is the operator's own note, not a value.
			return append(out, line[i:]...)
		case at(line, i, `"""`), at(line, i, "'''"):
			// A multi-line string opens here and one line cannot tell where it
			// ends, so the rest of the line is treated as a single value:
			// half-masking a secret is worse than showing the delimiters.
			out = append(out, line[i:i+3]...)
			return append(out, mask(name, string(line[i+3:]))...)
		case c == '"', c == '\'':
			end := skipBasic(line, i)
			if c == '\'' {
				end = skipLiteral(line, i)
			}
			// An unterminated string has no closing quote to put back.
			inner := end
			if inner > i+1 && line[inner-1] == c {
				inner--
			}
			out = append(out, c)
			out = append(out, mask(name, string(line[i+1:inner]))...)
			out = append(out, line[inner:end]...)
			i = end
		default:
			out = append(out, c)
			i++
		}
	}
	return out
}

func splice(src []byte, start, end int, repl []byte) []byte {
	out := make([]byte, 0, len(src)-(end-start)+len(repl))
	out = append(out, src[:start]...)
	out = append(out, repl...)
	return append(out, src[end:]...)
}

// dominantEOL reports the line ending the file already uses. TOML accepts
// CRLF, and a file that uses it must keep using it: a mixed file confuses the
// next person to open it in an editor that shows ^M.
func dominantEOL(src []byte) string {
	crlf := bytes.Count(src, []byte("\r\n"))
	lf := bytes.Count(src, []byte("\n")) - crlf
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

// Normalise converts submitted text to a file's line endings.
//
// This is not optional tidying. The HTML spec requires form submission to
// normalise <textarea> line breaks to CRLF, so *every* dashboard save arrives
// with CRLF regardless of what the file on disk uses — and a scanner
// comparing against "[ntfy]" would then never match a single line.
func Normalise(s string, src []byte) string {
	return string(normalise([]byte(s), dominantEOL(src)))
}

func normalise(b []byte, eol string) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
	if eol != "\n" {
		b = bytes.ReplaceAll(b, []byte("\n"), []byte(eol))
	}
	return b
}

// HasTableHeader reports whether submitted body text contains a table header,
// which the dashboard rejects: the header is the form's to own, not the
// textarea's. Catches both a renamed table and a pasted sub-table.
func HasTableHeader(body []byte) bool {
	for _, l := range scan(body) {
		if l.structural && l.kind == kindHeader {
			return true
		}
	}
	return false
}

// Verify checks a located span against the real TOML parser, which is the
// whole reason the scanner above is allowed to be pragmatic.
//
// The three regions of a correct split each parse on their own, and the middle
// defines exactly the target table. A span that landed inside a multi-line
// string, a multi-line array or an inline table breaks at least one of those,
// because the text either side of the cut is then syntactically truncated.
// The parser's opinion is independent of the scanner's, so this is a real
// check and not a restatement of the same assumption.
func Verify(src []byte, start, end int, id string) error {
	if start < 0 || end < start || end > len(src) {
		return fmt.Errorf("span [%d,%d) out of range for %d bytes", start, end, len(src))
	}
	var discard map[string]any
	if _, err := toml.Decode(string(src[:start]), &discard); err != nil {
		return fmt.Errorf("text before %q does not parse on its own: %w", id, err)
	}
	if _, err := toml.Decode(string(src[end:]), &discard); err != nil {
		return fmt.Errorf("text after %q does not parse on its own: %w", id, err)
	}
	var mid map[string]any
	if _, err := toml.Decode(string(src[start:end]), &mid); err != nil {
		return fmt.Errorf("the %q block does not parse on its own: %w", id, err)
	}
	if len(mid) != 1 {
		return fmt.Errorf("the %q block defines %d top-level tables, expected exactly 1", id, len(mid))
	}
	if _, ok := mid[id]; !ok {
		return fmt.Errorf("the located block defines something other than %q", id)
	}
	return nil
}

// VerifyNeighbours checks that an edit changed only the table it claimed to.
// Comment loss it cannot see — a comment has no semantics — but "you ate the
// neighbour" it catches every time.
func VerifyNeighbours(before, after []byte, id string) error {
	var b, a map[string]any
	if _, err := toml.Decode(string(before), &b); err != nil {
		return fmt.Errorf("original does not parse: %w", err)
	}
	if _, err := toml.Decode(string(after), &a); err != nil {
		return fmt.Errorf("result does not parse: %w", err)
	}
	for k, bv := range b {
		if k == id {
			continue
		}
		av, ok := a[k]
		if !ok {
			return fmt.Errorf("editing %q would delete the unrelated table %q", id, k)
		}
		if !equalAny(bv, av) {
			return fmt.Errorf("editing %q would change the unrelated table %q", id, k)
		}
	}
	for k := range a {
		if k == id {
			continue
		}
		if _, ok := b[k]; !ok {
			return fmt.Errorf("editing %q would add the unrelated table %q", id, k)
		}
	}
	return nil
}

// equalAny compares decoded TOML values. reflect.DeepEqual would do, but the
// decoded shape is only ever maps, slices and scalars, and this keeps the
// comparison explicit about what it is willing to see.
func equalAny(x, y any) bool {
	switch xv := x.(type) {
	case map[string]any:
		yv, ok := y.(map[string]any)
		if !ok || len(xv) != len(yv) {
			return false
		}
		for k, v := range xv {
			w, ok := yv[k]
			if !ok || !equalAny(v, w) {
				return false
			}
		}
		return true
	case []any:
		yv, ok := y.([]any)
		if !ok || len(xv) != len(yv) {
			return false
		}
		for i := range xv {
			if !equalAny(xv[i], yv[i]) {
				return false
			}
		}
		return true
	default:
		return x == y
	}
}
