package dragoman

import (
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

// parseSource is a model's answer for the language a message is in, or
// ok=false for English, no language at all, or something that isn't one.
func parseSource(s string) (tag language.Tag, ok bool) {
	tag, err := language.Parse(strings.TrimSpace(s))
	if err != nil {
		return tag, false
	}
	// "und" has English as its likeliest base, so it goes with it.
	switch baseOf(tag) {
	case "en", "mul", "zxx", "mis":
		return tag, false
	}
	return tag, true
}

func baseOf(tag language.Tag) string {
	b, _ := tag.Base()
	return b.String()
}

// flag is the flag of where tag's language is from: the region the tag
// names, else the one most of its speakers are in. A language with no
// country gets a globe and its name.
func flag(tag language.Tag) string {
	r, _ := tag.Region()
	if !r.IsCountry() {
		base, _ := tag.Base()
		r, _ = language.Make(base.String()).Region()
	}
	if code := r.String(); r.IsCountry() && len(code) == 2 {
		const regionalA = 0x1F1E6
		return string([]rune{regionalA + rune(code[0]-'A'), regionalA + rune(code[1]-'A')})
	}
	return "\U0001F310 " + languageName(tag)
}

func languageName(tag language.Tag) string {
	if n := display.English.Languages().Name(tag); n != "" {
		return n
	}
	return tag.String()
}
