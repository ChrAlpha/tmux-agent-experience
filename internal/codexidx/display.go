package codexidx

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// displayMax is how many grapheme clusters of a thread name the Codex 0.166
// terminal title shows; longer names keep displayMax-3 and end in "...".
const displayMax = 48

// Display returns a thread name as the Codex terminal title shows it: trimmed
// and truncated by grapheme cluster. "" means the title shows no name.
func Display(name string) string {
	name = strings.TrimSpace(name)
	clusters := 0
	cut := -1 // byte offset after displayMax-3 clusters
	for i, r := range name {
		if !extends(name[:i], r) {
			if clusters == displayMax-3 {
				cut = i
			}
			clusters++
			if clusters > displayMax {
				return name[:cut] + "..."
			}
		}
	}
	return name
}

// extends approximates the Unicode rule that keeps r in the grapheme cluster
// before it: combining marks, joiners and their successors, variation
// selectors, emoji modifiers and tags, and the second of a flag's regional
// indicators. It covers what thread names carry; a name it splits differently
// from Codex shows in a form no lookup matches, which proves nothing.
func extends(before string, r rune) bool {
	if before == "" {
		return false
	}
	prev, _ := utf8.DecodeLastRuneInString(before)
	switch {
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc),
		r == '\u200d', prev == '\u200d',
		r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0100 && r <= 0xe01ef,
		r >= 0x1f3fb && r <= 0x1f3ff, r >= 0xe0020 && r <= 0xe007f:
		return true
	case regional(r) && regional(prev):
		// Pairs from the start of the run form flags.
		n := 0
		for s := before; s != ""; {
			p, size := utf8.DecodeLastRuneInString(s)
			if !regional(p) {
				break
			}
			n++
			s = s[:len(s)-size]
		}
		return n%2 == 1
	}
	return false
}

func regional(r rune) bool { return r >= 0x1f1e6 && r <= 0x1f1ff }
