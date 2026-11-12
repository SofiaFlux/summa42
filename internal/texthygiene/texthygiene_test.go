package texthygiene_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/SofiaFlux/summa42/internal/texthygiene"
)

func TestBidiAndZeroWidthRejected(t *testing.T) {
	hidden := map[string]string{
		"right-to-left override":    "access\u202egranted",
		"left-to-right embedding":   "a\u202ab",
		"isolate":                   "a\u2066b\u2069",
		"zero width space":          "ACC\u200bEPT",
		"zero width joiner":         "a\u200db",
		"zero width non-joiner":     "a\u200cb",
		"word joiner":               "a\u2060b",
		"byte order mark":           "\ufeffpackage p",
		"soft hyphen":               "pass\u00adword",
		"left-to-right mark":        "a\u200eb",
		"tag character":             "a\U000e0041b",
		"line separator":            "a\u2028b",
		"paragraph separator":       "a\u2029b",
		"escape":                    "a\x1b[2Kb",
		"backspace":                 "a\bb",
		"form feed":                 "a\fb",
		"delete":                    "a\x7fb",
		"C1 control":                "a\u0085b",
		"NUL":                       "a\x00b",
		"invalid UTF-8":             "a\xffb",
		"interlinear annotation":    "a\ufff9b",
		"arabic letter mark":        "a\u061cb",
		"mongolian vowel separator": "a\u180eb",
	}
	for name, text := range hidden {
		finding, found := texthygiene.Hidden(text)
		if !found {
			t.Errorf("%s: not detected in %q", name, text)
			continue
		}
		if finding.Offset < 0 || finding.Offset >= len(text) || finding.Name == "" {
			t.Errorf("%s: finding %+v does not locate the character", name, finding)
		}
		if err := texthygiene.Check("field", text); err == nil || !strings.Contains(err.Error(), "field") {
			t.Errorf("%s: Check = %v, want an error naming the field", name, err)
		}
	}
}

func TestCleanOutputPasses(t *testing.T) {
	for _, text := range []string{
		"",
		"plain ASCII text.",
		"tabs\tand\nnewlines\r\nare text",
		"Zażółć gęślą jaźń — „cudzysłów” i myślnik",
		"日本語のテキスト, العربية, עברית",
		"emoji without a joiner: 🚀 ✅ and a variation selector ❤️",
		"non-breaking\u00a0space and narrow\u202fspace are visible spacing",
		"package p\n\nfunc f() string {\n\treturn \"\\u202e is fine when escaped\"\n}\n",
	} {
		if finding, found := texthygiene.Hidden(text); found {
			t.Errorf("%q: reported %+v", text, finding)
		}
		if err := texthygiene.Check("field", text); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
}

func TestFindingLocatesTheFirstHiddenCharacter(t *testing.T) {
	finding, found := texthygiene.Hidden("ok\nline two \u202e then \u200b")
	if !found || finding.Rune != 0x202e || finding.Offset != len("ok\nline two ") || finding.Line != 2 {
		t.Fatalf("finding = %+v, found = %v", finding, found)
	}
	err := texthygiene.Check("plan summary", "x\u200by")
	if err == nil || !strings.Contains(err.Error(), "U+200B") || !strings.Contains(err.Error(), "plan summary") {
		t.Fatalf("error = %v", err)
	}
}

func TestHiddenRunesCountsPerCodePoint(t *testing.T) {
	got := texthygiene.HiddenRunes("a\u200cb\u200c\ufeffc\xff\n")
	if got['\u200c'] != 2 || got['\ufeff'] != 1 || got[utf8.RuneError] != 1 || len(got) != 3 {
		t.Errorf("HiddenRunes = %v", got)
	}
	if len(texthygiene.HiddenRunes("plain\ttext\n")) != 0 {
		t.Error("plain text reported")
	}
}
