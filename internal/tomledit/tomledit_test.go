package tomledit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// exampleFiles are the shipped operational files. They are the reason this
// package exists and the reason its boundary rules look the way they do, so
// they are also the fixtures: a rule that survives here survives the files a
// user actually edits on day one.
func exampleFiles(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range []string{"channels.toml", "keys.toml", "hubbub.toml"} {
		p := filepath.Join("..", "..", "example", name)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		out[name] = b
	}
	return out
}

// TestRoundTripIsByteIdentical is the highest-value test in the package:
// replacing a block with itself must not move a single byte. It runs over
// every table in every shipped example file.
func TestRoundTripIsByteIdentical(t *testing.T) {
	for name, src := range exampleFiles(t) {
		ids := IDs(src)
		if len(ids) == 0 {
			t.Logf("%s: no top-level tables, skipping", name)
			continue
		}
		for _, id := range ids {
			block, err := Block(src, id)
			if err != nil {
				t.Fatalf("%s: Block(%q): %v", name, id, err)
			}
			got, err := ReplaceBlock(src, id, block)
			if err != nil {
				t.Fatalf("%s: ReplaceBlock(%q): %v", name, id, err)
			}
			if !bytes.Equal(got, src) {
				t.Errorf("%s: ReplaceBlock(%q, Block(...)) changed the file", name, id)
			}

			body, err := Body(src, id)
			if err != nil {
				t.Fatalf("%s: Body(%q): %v", name, id, err)
			}
			got, err = ReplaceBody(src, id, body)
			if err != nil {
				t.Fatalf("%s: ReplaceBody(%q): %v", name, id, err)
			}
			if !bytes.Equal(got, src) {
				t.Errorf("%s: ReplaceBody(%q, Body(...)) changed the file", name, id)
			}
		}
	}
}

// TestTrailingDocumentationSurvives is the regression test for the bug that
// reshaped this package. example/channels.toml is [ntfy] followed by forty
// lines of commented-out [email] and [standby] documentation running to EOF; a
// "block spans to the next table or EOF" rule deletes every one of them and
// still produces valid TOML.
func TestTrailingDocumentationSurvives(t *testing.T) {
	src := exampleFiles(t)["channels.toml"]

	block, err := Block(src, "ntfy")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if got := string(block); !strings.Contains(got, `token = ""`) {
		t.Errorf("ntfy block is missing its last value:\n%s", got)
	}
	for _, leaked := range []string{"[email]", "smtp.mail.me.com", "[standby]", "iCloud"} {
		if strings.Contains(string(block), leaked) {
			t.Errorf("ntfy block swallowed trailing documentation containing %q:\n%s", leaked, block)
		}
	}

	// The destructive operation, against the real file.
	out, err := RemoveBlock(src, "ntfy")
	if err != nil {
		t.Fatalf("RemoveBlock: %v", err)
	}
	for _, kept := range []string{
		"# [email]",
		"# password = \"abcd-efgh-ijkl-mnop\"",
		"debug this at 2am",
		"# [standby]",
		"# Channel instances.", // the file preamble
	} {
		if !strings.Contains(string(out), kept) {
			t.Errorf("RemoveBlock(ntfy) deleted %q from the file", kept)
		}
	}
	if strings.Contains(string(out), `topic = "CHANGE-ME-unguessable-topic"`) {
		t.Error("RemoveBlock(ntfy) left the ntfy body behind")
	}
}

func TestKeysFileRotationHowToSurvives(t *testing.T) {
	src := exampleFiles(t)["keys.toml"]
	out, err := RemoveBlock(src, "dev")
	if err != nil {
		t.Fatalf("RemoveBlock: %v", err)
	}
	for _, kept := range []string{
		"# One entry per caller.",
		"# key also accepts an array",
		"# [backup-host]",
	} {
		if !strings.Contains(string(out), kept) {
			t.Errorf("RemoveBlock(dev) deleted %q", kept)
		}
	}
	if strings.Contains(string(out), "nh_dev_key_change_me") {
		t.Error("RemoveBlock(dev) left the key behind")
	}
}

// TestFilePreambleIsNotBlockLeading covers the one-blank-line hazard: delete
// the blank between a file's preamble and its first table and a naive
// leading-comment-run rule hands the whole preamble to that table, where
// RemoveBlock then eats it.
func TestFilePreambleIsNotBlockLeading(t *testing.T) {
	src := []byte(`# File preamble that belongs to nobody.
# Second preamble line.
[dev]
key = "0123456789abcdef01"
channels = []
`)
	block, err := Block(src, "dev")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if strings.Contains(string(block), "preamble") {
		t.Errorf("block claimed the file preamble:\n%s", block)
	}
	out, err := RemoveBlock(src, "dev")
	if err != nil {
		t.Fatalf("RemoveBlock: %v", err)
	}
	if !strings.Contains(string(out), "File preamble") {
		t.Errorf("RemoveBlock deleted the file preamble:\n%s", out)
	}
}

func TestLeadingCommentRunBelongsToTheBlock(t *testing.T) {
	src := []byte(`[first]
a = 1

# This documents second.
# Still documenting second.
[second]
b = 2
`)
	block, err := Block(src, "second")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if !strings.Contains(string(block), "This documents second") {
		t.Errorf("block dropped its leading comment run:\n%s", block)
	}
	if strings.Contains(string(block), "a = 1") {
		t.Errorf("block reached into the previous table:\n%s", block)
	}
}

func TestCommentSeparatedByBlankLineIsNotBlockLeading(t *testing.T) {
	src := []byte(`[first]
a = 1

# Detached note.

[second]
b = 2
`)
	block, err := Block(src, "second")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if strings.Contains(string(block), "Detached note") {
		t.Errorf("a blank-separated comment was claimed as block-leading:\n%s", block)
	}
}

// TestMultilineStringsAreNotHeaders is the case a line scanner cannot handle:
// the planned exec adapter carries a shell script whose body contains lines
// starting with '[' and '#'.
func TestMultilineStringsAreNotHeaders(t *testing.T) {
	src := []byte(`[deploy]
type = "exec"
script = '''
#!/bin/sh
[ -z "$1" ] && exit 1
echo "[not a table]"
'''
after = true

[other]
x = 1
`)
	block, err := Block(src, "deploy")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	for _, want := range []string{"#!/bin/sh", `[ -z "$1" ]`, "'''", "after = true"} {
		if !strings.Contains(string(block), want) {
			t.Errorf("block truncated inside the multi-line string; missing %q:\n%s", want, block)
		}
	}
	if strings.Contains(string(block), "x = 1") {
		t.Errorf("block ran past its own table:\n%s", block)
	}
	if ids := IDs(src); len(ids) != 2 || ids[0] != "deploy" || ids[1] != "other" {
		t.Errorf("IDs = %v, want [deploy other]", ids)
	}
}

func TestMultilineBasicStringWithEscapesAndContinuation(t *testing.T) {
	src := []byte(`[a]
s = """
line one \
continued
[still inside]
escaped \""" quotes
"""
k = 1

[b]
y = 2
`)
	if ids := IDs(src); len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("IDs = %v, want [a b]", ids)
	}
	block, err := Block(src, "a")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if !strings.Contains(string(block), "k = 1") {
		t.Errorf("block ended early:\n%s", block)
	}
	if strings.Contains(string(block), "y = 2") {
		t.Errorf("block ran into [b]:\n%s", block)
	}
}

func TestMultilineAndNestedArrays(t *testing.T) {
	src := []byte(`[email]
to = [
  "a@example.com",
  "b@example.com",
]
matrix = [
  [1, 2],
  [3, 4],
]
tls = "starttls"

[next]
z = 0
`)
	block, err := Block(src, "email")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if !strings.Contains(string(block), `tls = "starttls"`) {
		t.Errorf("block ended inside an array:\n%s", block)
	}
	if strings.Contains(string(block), "z = 0") {
		t.Errorf("block ran into [next]:\n%s", block)
	}
	if ids := IDs(src); len(ids) != 2 {
		t.Errorf("IDs = %v, want 2 entries", ids)
	}
}

func TestStringsContainingHashAreNotComments(t *testing.T) {
	src := []byte(`[ntfy]
topic = "alerts # prod"
inline = { a = "#x", b = "[y]" }
token = "z"
`)
	block, err := Block(src, "ntfy")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if !strings.Contains(string(block), `token = "z"`) {
		t.Errorf("block ended early:\n%s", block)
	}
}

func TestHeaderVariants(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		id     string
		wantOK bool
	}{
		{"trailing comment", "[ntfy]  # the phone\nx = 1\n", "ntfy", true},
		{"internal whitespace", "[ ntfy ]\nx = 1\n", "ntfy", true},
		{"tab indented", "\t[ntfy]\nx = 1\n", "ntfy", true},
		{"quoted dotted id", "[\"ops.team\"]\nx = 1\n", "ops.team", true},
		{"prefix collision", "[dev]\nx = 1\n\n[dev2]\ny = 2\n", "dev", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Block([]byte(tc.src), tc.id)
			if tc.wantOK && err != nil {
				t.Fatalf("Block(%q) = %v, want success", tc.id, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("Block(%q) succeeded, want failure", tc.id)
			}
		})
	}
}

// A quoted header ["ops.team"] and a sub-table header [ops.team] look alike
// once decoded but mean entirely different things. Confusing them would let an
// edit land on the wrong entity.
func TestQuotedIDIsNotASubTable(t *testing.T) {
	sub := []byte("[ops]\na = 1\n\n[ops.team]\nb = 2\n")
	if _, err := Block(sub, "ops.team"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("sub-table header: err = %v, want ErrUnsupported", err)
	}
	if _, err := Block(sub, "ops"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("parent of a sub-table: err = %v, want ErrUnsupported", err)
	}
}

func TestPrefixCollisionDoesNotFalseMatch(t *testing.T) {
	src := []byte("[dev]\nx = 1\n\n[dev2]\ny = 2\n")
	block, err := Block(src, "dev")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if strings.Contains(string(block), "y = 2") {
		t.Errorf("[dev] matched [dev2]:\n%s", block)
	}
}

func TestArrayOfTablesIsRefused(t *testing.T) {
	src := []byte("[[product]]\nname = \"a\"\n\n[[product]]\nname = \"b\"\n")
	if _, err := Block(src, "product"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
	if ids := IDs(src); len(ids) != 0 {
		t.Errorf("IDs = %v, want none (array tables are not simple)", ids)
	}
}

// A caller defined entirely with dotted keys has no header to locate. The
// error has to say so: silently returning not-found would make the dashboard
// offer to create a duplicate.
func TestDottedKeyEntryHasNoHeader(t *testing.T) {
	src := []byte("dev.key = \"0123456789abcdef01\"\ndev.channels = []\n")
	_, err := Block(src, "dev")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCRLFFiles(t *testing.T) {
	src := []byte("# preamble\r\n\r\n[ntfy]\r\ntype = \"ntfy\"\r\ntopic = \"t\"\r\n\r\n# trailing note\r\n")
	block, err := Block(src, "ntfy")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if !strings.Contains(string(block), "topic") {
		t.Errorf("CRLF block not located:\n%q", block)
	}
	if strings.Contains(string(block), "trailing note") {
		t.Errorf("CRLF block swallowed the trailing comment:\n%q", block)
	}

	// A textarea always submits CRLF; the file here is CRLF too, so the
	// result must stay consistently CRLF rather than going mixed.
	out, err := ReplaceBody(src, "ntfy", []byte("type = \"ntfy\"\r\ntopic = \"new\"\r\n"))
	if err != nil {
		t.Fatalf("ReplaceBody: %v", err)
	}
	if bytes.Contains(bytes.ReplaceAll(out, []byte("\r\n"), nil), []byte("\n")) {
		t.Errorf("result has bare LF among CRLF:\n%q", out)
	}
	if !strings.Contains(string(out), "new") {
		t.Errorf("replacement did not land:\n%q", out)
	}
}

// The guaranteed case: an LF file edited through a browser form, which the
// HTML spec requires to submit CRLF.
func TestTextareaCRLFIsNormalisedIntoAnLFFile(t *testing.T) {
	src := []byte("[ntfy]\ntype = \"ntfy\"\ntopic = \"old\"\n")
	out, err := ReplaceBody(src, "ntfy", []byte("type = \"ntfy\"\r\ntopic = \"new\"\r\n"))
	if err != nil {
		t.Fatalf("ReplaceBody: %v", err)
	}
	if bytes.Contains(out, []byte("\r")) {
		t.Errorf("CR survived into an LF file:\n%q", out)
	}
	if !strings.Contains(string(out), `topic = "new"`) {
		t.Errorf("replacement did not land:\n%q", out)
	}
}

func TestNoTrailingNewline(t *testing.T) {
	src := []byte("[a]\nx = 1")
	out := AppendBlock(src, []byte("[b]\ny = 2\n"))
	if strings.Contains(string(out), "x = 1[b]") {
		t.Errorf("appended block was glued onto the last value:\n%q", out)
	}
	if ids := IDs(out); len(ids) != 2 {
		t.Errorf("IDs = %v, want 2", ids)
	}
	var v map[string]any
	if _, err := toml.Decode(string(out), &v); err != nil {
		t.Errorf("result does not parse: %v\n%q", err, out)
	}
}

func TestBOMSurvives(t *testing.T) {
	src := []byte("\ufeff[a]\nx = 1\n")
	out, err := ReplaceBody(src, "a", []byte("x = 2\n"))
	if err != nil {
		t.Fatalf("ReplaceBody: %v", err)
	}
	if !bytes.HasPrefix(out, []byte("\ufeff")) {
		t.Errorf("BOM lost:\n%q", out)
	}
	if bytes.Count(out, []byte("\ufeff")) != 1 {
		t.Errorf("BOM duplicated:\n%q", out)
	}
}

func TestDegenerateFiles(t *testing.T) {
	for _, src := range [][]byte{nil, []byte(""), []byte("\n\n"), []byte("# only comments\n")} {
		if ids := IDs(src); len(ids) != 0 {
			t.Errorf("IDs(%q) = %v, want none", src, ids)
		}
		if _, err := Block(src, "x"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Block(%q) err = %v, want ErrNotFound", src, err)
		}
		out := AppendBlock(src, []byte("[x]\na = 1\n"))
		if ids := IDs(out); len(ids) != 1 {
			t.Errorf("AppendBlock onto %q gave IDs %v, want [x]", src, ids)
		}
	}
}

func TestAppendBlockDoesNotAdoptTrailingComments(t *testing.T) {
	src := exampleFiles(t)["channels.toml"]
	out := AppendBlock(src, []byte("[slack]\ntype = \"ntfy\"\ntopic = \"s\"\n"))
	block, err := Block(out, "slack")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if strings.Contains(string(block), "[standby]") || strings.Contains(string(block), "iCloud") {
		t.Errorf("appended block adopted the file's trailing documentation:\n%s", block)
	}
	if !strings.Contains(string(out), "# [standby]") {
		t.Error("append lost the trailing documentation")
	}
}

func TestSetKeyInBlock(t *testing.T) {
	src := []byte(`# preamble

# documents ntfy
[ntfy]
type = "ntfy"
# port is commented out on purpose
# enabled = false
topic = "t"

# trailing
`)
	t.Run("existing key keeps its position", func(t *testing.T) {
		out, err := SetKeyInBlock(src, "ntfy", "topic", `"new"`)
		if err != nil {
			t.Fatalf("SetKeyInBlock: %v", err)
		}
		if !strings.Contains(string(out), `topic = "new"`) {
			t.Errorf("not set:\n%s", out)
		}
		if !strings.Contains(string(out), "# port is commented out on purpose") {
			t.Error("in-block comment lost")
		}
		if !strings.Contains(string(out), "# documents ntfy") {
			t.Error("leading comment lost")
		}
		if !strings.Contains(string(out), "# trailing") {
			t.Error("trailing comment lost")
		}
	})

	t.Run("commented-out key is not the key", func(t *testing.T) {
		out, err := SetKeyInBlock(src, "ntfy", "enabled", "false")
		if err != nil {
			t.Fatalf("SetKeyInBlock: %v", err)
		}
		if !strings.Contains(string(out), "# enabled = false") {
			t.Error("the commented-out line was overwritten")
		}
		var v struct {
			Ntfy struct {
				Enabled *bool `toml:"enabled"`
			} `toml:"ntfy"`
		}
		if _, err := toml.Decode(string(out), &v); err != nil {
			t.Fatalf("result does not parse: %v\n%s", err, out)
		}
		if v.Ntfy.Enabled == nil || *v.Ntfy.Enabled {
			t.Errorf("enabled did not take effect: %v\n%s", v.Ntfy.Enabled, out)
		}
	})

	t.Run("absent key is inserted", func(t *testing.T) {
		out, err := SetKeyInBlock(src, "ntfy", "server", `"https://example.com"`)
		if err != nil {
			t.Fatalf("SetKeyInBlock: %v", err)
		}
		var v struct {
			Ntfy struct {
				Server string `toml:"server"`
				Topic  string `toml:"topic"`
			} `toml:"ntfy"`
		}
		if _, err := toml.Decode(string(out), &v); err != nil {
			t.Fatalf("result does not parse: %v\n%s", err, out)
		}
		if v.Ntfy.Server != "https://example.com" || v.Ntfy.Topic != "t" {
			t.Errorf("got %+v", v.Ntfy)
		}
	})
}

// A missed line here is a leaked password; a false positive corrupts a script
// body. Both directions matter.
func TestKeyLines(t *testing.T) {
	body := []byte(`type = "smtp"
# password = "commented out, not an assignment"
password = "s3cret"
port = 587
script = '''
not = "an assignment"
'''
to = [
  "a@b.c",
]
tls = "starttls"  # trailing comment
`)
	got := KeyLines(body)
	var names []string
	for _, k := range got {
		names = append(names, k.Name)
	}
	want := []string{"type", "password", "port", "script", "to", "tls"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}

	for _, k := range got {
		if k.Name == "password" {
			if k.Value != `"s3cret"` {
				t.Errorf("password value = %q", k.Value)
			}
			if string(body[k.Start:k.End]) != "password = \"s3cret\"\n" {
				t.Errorf("password span = %q", body[k.Start:k.End])
			}
		}
	}
}

// A mask that reformats the line around the value is a mask that makes a diff
// unreadable, and a value it walks past is a credential in the DOM.
func TestMaskStrings(t *testing.T) {
	// Blanks every string it is offered, so the tests read as "what counted as
	// a value here, and what came back byte for byte".
	blank := func(name, raw string) string { return "X" }

	cases := []struct {
		line, want string
	}{
		// Array shape, spacing and the trailing comment all survive.
		{`key = ["one", "two"]  # rotating`, `key = ["X", "X"]  # rotating`},
		{`key   =    "one"`, `key   =    "X"`},
		{`key = 'literal'`, `key = 'X'`},
		{`key = ""`, `key = "X"`},
		// A continuation line of a multi-line array holds values with no name
		// attached to them.
		{`  "one",`, `  "X",`},
		// Not values: a header, a comment, a blank, a number.
		{`[dev]`, `[dev]`},
		{`["quoted-id"]`, `["quoted-id"]`},
		{`# key = ["one"]`, `# key = ["one"]`},
		{``, ``},
		{`max_per_hour = 5`, `max_per_hour = 5`},
		// A '#' inside a string is part of the value, not the start of a comment.
		{`password = "a#b"`, `password = "X"`},
		// An unterminated string is what a truncated write leaves behind; the
		// parser will reject the file, but the value still must not be shown.
		{`key = "one`, `key = "X`},
		// Where a multi-line string ends is unknowable from one line, so the
		// rest of the line is one value rather than three guesses.
		{`key = """one`, `key = """X`},
	}
	for _, c := range cases {
		if got := string(MaskStrings([]byte(c.line), blank)); got != c.want {
			t.Errorf("MaskStrings(%q) = %q, want %q", c.line, got, c.want)
		}
	}

	// The name is what a caller decides policy on, so it has to arrive.
	var names []string
	MaskStrings([]byte(`key = ["one", "two"]`), func(name, raw string) string {
		names = append(names, name)
		return raw
	})
	if len(names) != 2 || names[0] != "key" || names[1] != "key" {
		t.Errorf("names = %v, want key twice", names)
	}

	// Handing the raw text back is a no-op, whatever the line contains.
	for _, line := range []string{`key = ["one", "two"]  # note`, `password = 'a#b'`, `  "one",`} {
		keep := func(name, raw string) string { return raw }
		if got := string(MaskStrings([]byte(line), keep)); got != line {
			t.Errorf("identity mask changed %q to %q", line, got)
		}
	}
}

func TestHasTableHeader(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{"type = \"ntfy\"\ntopic = \"t\"\n", false},
		{"[ntfy2]\ntype = \"ntfy\"\n", true},
		{"type = \"ntfy\"\n[sub.table]\n", true},
		{"script = '''\n[ -z \"$1\" ]\n'''\n", false},
		{"to = [\n  \"a@b\",\n]\n", false},
		{"# [commented]\ntype = \"ntfy\"\n", false},
	}
	for _, tc := range tests {
		if got := HasTableHeader([]byte(tc.body)); got != tc.want {
			t.Errorf("HasTableHeader(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestVerifyAcceptsRealSpans(t *testing.T) {
	for name, src := range exampleFiles(t) {
		for _, id := range IDs(src) {
			start, end, err := Span(src, id)
			if err != nil {
				t.Fatalf("%s: Span(%q): %v", name, id, err)
			}
			if err := Verify(src, start, end, id); err != nil {
				t.Errorf("%s: Verify(%q): %v", name, id, err)
			}
		}
	}
}

// Verify exists so the scanner may be pragmatic. These are the spans a wrong
// scanner would produce; each must be caught by the parser rather than
// written.
func TestVerifyRejectsBadSpans(t *testing.T) {
	src := []byte("[a]\ns = \"\"\"\ntext\n\"\"\"\nk = 1\n\n[b]\ny = 2\n")
	start, end, err := Span(src, "a")
	if err != nil {
		t.Fatalf("Span: %v", err)
	}
	if err := Verify(src, start, end, "a"); err != nil {
		t.Fatalf("the correct span was rejected: %v", err)
	}

	t.Run("span cuts inside a multi-line string", func(t *testing.T) {
		cut := bytes.Index(src, []byte("text"))
		if err := Verify(src, start, cut, "a"); err == nil {
			t.Error("a span ending inside a multi-line string was accepted")
		}
	})
	t.Run("span swallows the next table", func(t *testing.T) {
		if err := Verify(src, start, len(src), "a"); err == nil {
			t.Error("a span covering two tables was accepted")
		}
	})
	t.Run("span names the wrong table", func(t *testing.T) {
		bStart, bEnd, err := Span(src, "b")
		if err != nil {
			t.Fatalf("Span(b): %v", err)
		}
		if err := Verify(src, bStart, bEnd, "a"); err == nil {
			t.Error("a span defining [b] was accepted as [a]")
		}
	})
	t.Run("out of range", func(t *testing.T) {
		if err := Verify(src, 0, len(src)+1, "a"); err == nil {
			t.Error("an out-of-range span was accepted")
		}
	})
}

func TestVerifyNeighbours(t *testing.T) {
	src := []byte("[a]\nx = 1\n\n[b]\ny = 2\n")

	t.Run("editing only the target passes", func(t *testing.T) {
		out, err := ReplaceBody(src, "a", []byte("x = 99\n"))
		if err != nil {
			t.Fatalf("ReplaceBody: %v", err)
		}
		if err := VerifyNeighbours(src, out, "a"); err != nil {
			t.Errorf("legitimate edit rejected: %v", err)
		}
	})
	t.Run("eating the neighbour is caught", func(t *testing.T) {
		mangled := []byte("[a]\nx = 1\n")
		if err := VerifyNeighbours(src, mangled, "a"); err == nil {
			t.Error("deleting [b] while editing [a] was accepted")
		}
	})
	t.Run("changing the neighbour is caught", func(t *testing.T) {
		mangled := []byte("[a]\nx = 1\n\n[b]\ny = 3\n")
		if err := VerifyNeighbours(src, mangled, "a"); err == nil {
			t.Error("changing [b] while editing [a] was accepted")
		}
	})
	t.Run("adding a neighbour is caught", func(t *testing.T) {
		mangled := []byte("[a]\nx = 1\n\n[b]\ny = 2\n\n[c]\nz = 3\n")
		if err := VerifyNeighbours(src, mangled, "a"); err == nil {
			t.Error("adding [c] while editing [a] was accepted")
		}
	})
}

func TestRemoveBlockLeavesOneSeparator(t *testing.T) {
	src := []byte("[a]\nx = 1\n\n[b]\ny = 2\n\n[c]\nz = 3\n")
	out, err := RemoveBlock(src, "b")
	if err != nil {
		t.Fatalf("RemoveBlock: %v", err)
	}
	if strings.Contains(string(out), "\n\n\n") {
		t.Errorf("removal left a double gap:\n%q", out)
	}
	if ids := IDs(out); len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
		t.Errorf("IDs = %v, want [a c]", ids)
	}
	var v map[string]any
	if _, err := toml.Decode(string(out), &v); err != nil {
		t.Errorf("result does not parse: %v\n%q", err, out)
	}
}

func TestOperationsOnMissingTable(t *testing.T) {
	src := []byte("[a]\nx = 1\n")
	if _, err := Block(src, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Block: %v", err)
	}
	if _, err := Body(src, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Body: %v", err)
	}
	if _, err := ReplaceBody(src, "nope", []byte("y = 2\n")); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReplaceBody: %v", err)
	}
	if _, err := RemoveBlock(src, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveBlock: %v", err)
	}
	if _, err := SetKeyInBlock(src, "nope", "k", "1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetKeyInBlock: %v", err)
	}
}

func TestHeaderOnlyTable(t *testing.T) {
	src := []byte("[a]\n\n[b]\ny = 2\n")
	block, err := Block(src, "a")
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if strings.Contains(string(block), "y = 2") {
		t.Errorf("header-only block ran into [b]:\n%q", block)
	}
	body, err := Body(src, "a")
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if strings.TrimSpace(string(body)) != "" {
		t.Errorf("body = %q, want empty", body)
	}
}
