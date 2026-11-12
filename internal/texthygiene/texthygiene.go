// Package texthygiene rejects text that does not show what it contains.
//
// Model output is read by another model (a reviewer) and by the Owner (a
// plan, a diff, a pull request). Invisible and bidirectional control
// characters let text display one thing and mean another: a hidden
// instruction, a reordered line of code, a verdict that only looks like
// "ACCEPT". Text that crosses from one role to another, or is published, must
// not contain them (development plan, section 2.5 and card M9-T8).
//
// This is a tripwire, not a proof. It removes one cheap channel. It does not
// detect look-alike letters, instructions written in plain sight, or anything
// encoded in visible text.
package texthygiene

import (
	"fmt"
	"unicode"
	"unicode/utf8"
)

// Finding locates one rejected character.
type Finding struct {
	Offset int    // byte offset in the text
	Line   int    // 1-based line
	Rune   rune   // the character; utf8.RuneError for an invalid byte
	Name   string // what kind of character it is
}

func (f Finding) String() string {
	if f.Rune == utf8.RuneError {
		return fmt.Sprintf("%s at byte %d (line %d)", f.Name, f.Offset, f.Line)
	}
	return fmt.Sprintf("%s U+%04X at byte %d (line %d)", f.Name, f.Rune, f.Offset, f.Line)
}

// classify names the kind of a rejected character, or returns "" for a
// character that is ordinary text.
func classify(r rune) string {
	switch {
	case r == '\t' || r == '\n' || r == '\r':
		return ""
	case r < 0x20 || r == 0x7f:
		return "control character"
	case r >= 0x80 && r <= 0x9f:
		return "control character"
	case r == 0x2028 || r == 0x2029:
		return "line or paragraph separator"
	case (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200e || r == 0x200f || r == 0x061c:
		return "bidirectional control character"
	case r == 0x180e:
		// Was a format character; still renders as nothing.
		return "invisible character"
	case unicode.Is(unicode.Cf, r):
		// Format characters: zero width space and joiners, word joiner, byte
		// order mark, soft hyphen, tag characters, interlinear annotations.
		return "invisible character"
	}
	return ""
}

// Hidden returns the first character in text that is invisible, a
// bidirectional control, a control character other than tab, line feed and
// carriage return, or not valid UTF-8.
func Hidden(text string) (Finding, bool) {
	line := 1
	for offset := 0; offset < len(text); {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if r == utf8.RuneError && size == 1 {
			return Finding{Offset: offset, Line: line, Rune: r, Name: "invalid UTF-8"}, true
		}
		if name := classify(r); name != "" {
			return Finding{Offset: offset, Line: line, Rune: r, Name: name}, true
		}
		if r == '\n' {
			line++
		}
		offset += size
	}
	return Finding{}, false
}

// Check returns an error naming the field and the first rejected character.
func Check(field, text string) error {
	if finding, found := Hidden(text); found {
		return fmt.Errorf("%s contains hidden content: %s; write it as an escape sequence if it is intended", field, finding)
	}
	return nil
}

// HiddenRunes counts every rejected character in text per code point. An
// invalid UTF-8 byte is counted under utf8.RuneError.
func HiddenRunes(text string) map[rune]int {
	counts := map[rune]int{}
	for offset := 0; offset < len(text); {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if (r == utf8.RuneError && size == 1) || classify(r) != "" {
			counts[r]++
		}
		offset += size
	}
	return counts
}
