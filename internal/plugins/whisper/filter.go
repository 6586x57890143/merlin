package whisper

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The free regex suite a whisper has to pass before anything else looks at
// it. Every hit here is a refusal, never a rewrite: this is text merlin is
// about to publish in a member's name, and there is no version of a leaked
// number or a link that is fine with a hole in it.
//
// Deliberately blunter than aimod's rung 1. That table has to be near-zero
// on false positives because a wrong hit there deletes somebody's message;
// a wrong hit here costs a restricted member one retry, and the audience is
// people Discord itself has decided not to trust with a message box. So
// links go, all of them, rather than only the phishing shapes, and so does
// anything shaped like a phone number.
//
// Go's regexp is RE2: no lookahead, no backtracking, so nothing here can be
// made to take exponential time by a crafted input.

const maxLen = 400

// filters are checked in order and the first hit wins. Each reason is a
// sentence for the member, because a refusal they cannot understand is one
// they will retry blindly.
var filters = []struct {
	reason  string
	pattern *regexp.Regexp
}{
	{
		reason:  "that looks like an email address",
		pattern: regexp.MustCompile(`(?i)\b[\w.+-]+@[\w-]+\.[a-z]{2,}\b`),
	},
	{
		reason: "links are not posted through whispers",
		// Any scheme, www., a bare host on any two-letter-or-longer TLD, or
		// a dotted IP. Not a list of bad hosts: no link at all, since the
		// audience is people Discord has decided not to trust with a message
		// box. "e.g." and "3.14" survive because a TLD needs two letters.
		pattern: regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://|\bwww\.|\b[a-z0-9-]+\.[a-z]{2,}\b|\b\d{1,3}(?:\.\d{1,3}){3}\b`),
	},
	{
		reason: "mentions are not posted through whispers",
		// The webhook already sends with every mention suppressed, so none
		// of these could ping; this stops them rendering as a highlight.
		pattern: regexp.MustCompile(`@everyone|@here|<@[!&]?\d+>`),
	},
	{
		reason: "that looks like a phone, card or ID number",
		// Nine or more digits with any separators between them. Long enough
		// to miss prices, years and ordinary counts, short enough to catch
		// every national phone format and a card number.
		pattern: regexp.MustCompile(`(?:\d[\s().-]*){9,}`),
	},
	{
		reason:  "telling somebody to hurt themselves is not posted",
		pattern: regexp.MustCompile(`(?i)\b(?:kys|k\s*y\s*s|kill\s+(?:your|ur)\s*self|neck\s+(?:your|ur)\s*self|off\s+(?:your|ur)\s*self|unalive\s+(?:your|ur)\s*self|end\s+(?:your|ur)\s*self|go\s+(?:die|hang\s+(?:your|ur)\s*self)|(?:drink|eat)\s+bleach|nobody\s+would\s+miss\s+you)\b`),
	},
	{
		reason: "that matches the child safety vocabulary",
		// Narrow, unlike aimod's neverSkipPattern, which is built to force a
		// scan and would refuse "my kids are loud" if it were used to refuse.
		// These are the terms with no innocent reading in a chat message.
		pattern: regexp.MustCompile(`(?i)\b(?:csam|c\.?p|lolis?|shotas?|jail\s?bait|child\s?(?:porn|sex)|kiddie\s?porn|cheese\s?pizza|toddlercon|pedo\s?(?:pride|rights|love))\b`),
	},
	{
		reason: "invisible or direction-changing characters are not posted",
		// Zero-width and bidi controls, the byte-order mark, soft hyphen and
		// the C0 range. Every one of them is a way to make the marker under
		// the message say something other than what it says, or to make the
		// text read differently from how it was typed.
		pattern: regexp.MustCompile(`[\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2060}-\x{2064}\x{FEFF}\x{00AD}\x{00}-\x{08}\x{0B}\x{0C}\x{0E}-\x{1F}\x{7F}]`),
	},
	{
		reason:  "stacked combining characters are not posted",
		pattern: regexp.MustCompile(`\p{M}{3,}`),
	},
}

// emote is a custom server emote, <:name:id> or <a:name:id>. Members may
// use them, so they are blanked before the suite runs: the id is a snowflake
// long enough to read as a card number, and nothing else in the tag is text
// a member wrote.
var emote = regexp.MustCompile(`<a?:\w{2,32}:\d{17,20}>`)

// check returns the refusal for text, or "" when the suite has nothing
// against it. The structural rules come first because they are the ones the
// marker's honesty depends on: a whisper is one line, and it cannot start
// with a markdown header, so the "-# whispered through merlin by @name"
// under it is always the last line and always the only subtext.
func check(text string) string {
	if text == "" {
		return "there is nothing to post"
	}
	if utf8.RuneCountInString(text) > maxLen {
		return fmt.Sprintf("that is over %d characters", maxLen)
	}
	if strings.ContainsAny(text, "\n\r") {
		return "one line at a time"
	}
	if strings.HasPrefix(text, "#") || strings.HasPrefix(text, "-#") {
		return "a whisper cannot start with a heading"
	}
	scanned := emote.ReplaceAllString(text, " ")
	for _, f := range filters {
		if f.pattern.MatchString(scanned) {
			return f.reason
		}
	}
	return ""
}
