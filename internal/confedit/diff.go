package confedit

import "strings"

// Op is what happened to one line in a diff.
type Op int

const (
	Same Op = iota
	Del
	Add
)

// DiffLine is one line of a rendered diff.
type DiffLine struct {
	Op   Op
	Text string
}

// Diff produces a line diff of two config files.
//
// It exists for one reason the structural guards cannot cover: comment loss.
// A comment has no semantics, so no parser and no deep-equal can tell that an
// edit ate one. Every operation here is rare, deliberate and has a human
// watching, so showing them what is about to change turns any residual
// boundary bug from silent into obvious — much the cheapest safety net in the
// design.
//
// Plain O(n*m) LCS. These files are hundreds of lines at most, and a
// hand-rolled Myers implementation would be more code to get wrong for no
// perceptible gain.
func Diff(before, after []byte) []DiffLine {
	a := splitLines(before)
	b := splitLines(after)

	// lcs[i][j] = length of the longest common subsequence of a[i:] and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	var out []DiffLine
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{Same, a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{Del, a[i]})
			i++
		default:
			out = append(out, DiffLine{Add, b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		out = append(out, DiffLine{Del, a[i]})
	}
	for ; j < len(b); j++ {
		out = append(out, DiffLine{Add, b[j]})
	}
	return out
}

// Changed reports whether a diff contains any change at all, so the UI can say
// "no change" rather than presenting an empty confirmation.
func Changed(d []DiffLine) bool {
	for _, l := range d {
		if l.Op != Same {
			return true
		}
	}
	return false
}

// Counts summarises a diff for a one-line headline — "-40 +2" is the shape
// that makes an accidental documentation deletion jump out.
func Counts(d []DiffLine) (added, deleted int) {
	for _, l := range d {
		switch l.Op {
		case Add:
			added++
		case Del:
			deleted++
		case Same:
		}
	}
	return added, deleted
}

func splitLines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
