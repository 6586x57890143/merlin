package aimod

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// fold takes the Unicode disguise off text before rung 1 reads it, so a slur
// spelled in fullwidth, math bold, small capitals, Cyrillic, circled letters,
// regional indicators, with accents or zalgo stacked on it, or with zero-width
// characters between its letters, is matched as the plain letters it renders
// as. Every rung-1 reader routes through it (redactSlurs, redactWords, squash,
// mustScan), so a new evasion is one table entry here rather than a fix in
// each.
//
// NFKD does most of the work: fullwidth, mathematical alphanumerics, circled
// and parenthesised letters, superscripts and ligatures all have compatibility
// decompositions to ASCII, and it splits an accented letter into the letter
// plus a combining mark. What it leaves alone is lookalikes from other
// scripts, which is what lookalikes is for, and the invisible characters,
// which are dropped.
//
// Combining marks are dropped only after a letter or digit, which is where an
// accent, a strikethrough or a zalgo stack sits. After anything else they are
// left, so an emoji keeps its variation selector and a ZWJ sequence stays
// joined. That matters because a hit publishes the folded text as the
// rewrite: a disguised slur costs the rest of the message its accents, which
// is a fair price, and should not cost it its emoji.
//
// ponytail: a hand table, not the full UTS #39 confusables list (~6k entries
// and a data file to vendor). It covers what renders as a Latin letter in
// Discord's font; add a row when a new one turns up.
func fold(s string) string {
	if isASCII(s) {
		return s
	}
	var b strings.Builder
	afterWord := false
	for _, r := range norm.NFKD.String(s) {
		if m, ok := lookalikes[r]; ok {
			r = m
		}
		switch {
		case invisible(r):
			continue
		case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
			if afterWord {
				continue
			}
		case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicators
			r = 'a' + r - 0x1F1E6
		case r >= 0x1F150 && r <= 0x1F169: // negative circled
			r = 'a' + r - 0x1F150
		case r >= 0x1F170 && r <= 0x1F189: // negative squared
			r = 'a' + r - 0x1F170
		}
		b.WriteRune(r)
		afterWord = isWordRune(r)
	}
	return b.String()
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// invisible is what renders as nothing, or as a blank, and so can sit between
// two letters of a slur without the reader seeing it. The Hangul fillers and
// the braille blank are the sneaky ones: Unicode calls them a letter and a
// symbol, so they never counted as a separator and simply broke the word.
// ZWJ (U+200D) and tag characters are left to the Cf rule above, since emoji
// sequences are built from them.
func invisible(r rune) bool {
	switch r {
	case 0x00AD, 0x034F, 0x061C, 0x115F, 0x1160, 0x17B4, 0x17B5, 0x180E,
		0x200B, 0x200C, 0x200E, 0x200F, 0x2060, 0x2061, 0x2062, 0x2063, 0x2064,
		0x2800, 0x3164, 0xFEFF, 0xFFA0:
		return true
	}
	return r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069
}

// lookalikes maps letters from other scripts to the Latin letter they render
// as. Lowercase targets: every reader matches case-insensitively.
var lookalikes = map[rune]rune{
	// Cyrillic
	'а': 'a', 'в': 'b', 'е': 'e', 'г': 'r', 'і': 'i', 'ј': 'j', 'к': 'k', 'м': 'm', 'н': 'h',
	'о': 'o', 'п': 'n', 'р': 'p', 'с': 'c', 'т': 't', 'у': 'y', 'х': 'x', 'ѕ': 's', 'ԁ': 'd',
	'ԛ': 'q', 'ԝ': 'w', 'ӏ': 'l', 'ь': 'b',
	'А': 'a', 'В': 'b', 'Е': 'e', 'І': 'i', 'Ј': 'j', 'К': 'k', 'М': 'm', 'Н': 'h', 'О': 'o',
	'Р': 'p', 'С': 'c', 'Т': 't', 'У': 'y', 'Х': 'x', 'Ѕ': 's',
	// Greek
	'α': 'a', 'β': 'b', 'γ': 'y', 'ε': 'e', 'η': 'n', 'ι': 'i', 'κ': 'k', 'μ': 'u', 'ν': 'v',
	'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x',
	'Α': 'a', 'Β': 'b', 'Ε': 'e', 'Ζ': 'z', 'Η': 'h', 'Ι': 'i', 'Κ': 'k', 'Μ': 'm', 'Ν': 'n',
	'Ο': 'o', 'Ρ': 'p', 'Τ': 't', 'Υ': 'y', 'Χ': 'x',
	// Armenian
	'ո': 'n', 'ս': 'u', 'օ': 'o', 'հ': 'h',
	// Latin small capitals and IPA, which NFKD leaves as they are
	'ᴀ': 'a', 'ʙ': 'b', 'ᴄ': 'c', 'ᴅ': 'd', 'ᴇ': 'e', 'ꜰ': 'f', 'ɢ': 'g', 'ʜ': 'h', 'ɪ': 'i',
	'ᴊ': 'j', 'ᴋ': 'k', 'ʟ': 'l', 'ᴍ': 'm', 'ɴ': 'n', 'ᴏ': 'o', 'ᴘ': 'p', 'ʀ': 'r', 'ꜱ': 's',
	'ᴛ': 't', 'ᴜ': 'u', 'ᴠ': 'v', 'ᴡ': 'w', 'ʏ': 'y', 'ᴢ': 'z',
	'ı': 'i', 'ȷ': 'j', 'ɑ': 'a', 'ɡ': 'g', 'ɩ': 'i', 'ŋ': 'n', 'ɨ': 'i', 'ʉ': 'u', 'ɵ': 'o',
	// Latin letters with a stroke, which have no decomposition to strip
	'ø': 'o', 'Ø': 'o', 'ł': 'l', 'Ł': 'l', 'đ': 'd', 'Đ': 'd', 'ħ': 'h', 'ŧ': 't', 'ƶ': 'z',
	'ǥ': 'g', 'ɇ': 'e', 'ɍ': 'r', 'ꝁ': 'k',
	// Latin letters with a hook or tail
	'ƒ': 'f', 'ɠ': 'g', 'ƙ': 'k', 'ɲ': 'n', 'ɳ': 'n', 'ƞ': 'n', 'ɾ': 'r', 'ɽ': 'r', 'ƭ': 't',
}
