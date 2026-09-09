package contest

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// codeShape describes a prize code without disclosing it.
//
// A mod has to be able to reject junk, and cannot be shown the code to do it:
// the sealing exists to keep it from everybody but the winner, and a
// moderator is not a smaller exception than anybody else. So this runs once,
// at pledge time, in the one function that already holds the plaintext, and
// what it returns is all any review surface ever sees.
//
// The prose bucket is the one that earns its keep, because a code is never
// prose. It is what separates "steam-shaped key, 17 characters" from "lol get
// rekt idiot", which is the actual submission this feature exists to catch.
//
// What it deliberately does not do is claim a code is real. Nothing short of
// redeeming it can, and a shape that looks right is exactly what a plausible
// fake would have.
func codeShape(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	if host := linkHost(code); host != "" {
		// The host, never the path. "discord.gift" says what kind of thing
		// this is; the token after the slash is the entire secret.
		return "link to " + host
	}
	if steamShaped(code) {
		return "steam-shaped key, " + strconv.Itoa(len(code)) + " characters"
	}
	if fields := strings.Fields(code); len(fields) > 1 {
		return plural(len(fields), "word") + " of prose"
	}
	return plural(len([]rune(code)), "character") + ", " + classOf(code)
}

// linkHost returns the host of a URL-looking code, or "".
//
// ponytail: a heuristic, not a validator. It takes the scheme-ful form
// through url.Parse and falls back to "a dot before the first slash" for the
// bare "discord.gift/xxx" people actually paste. Being wrong in either
// direction costs a mod one glance at the title; it is not a trust boundary,
// because nothing downstream acts on the answer.
func linkHost(code string) string {
	if strings.ContainsFunc(code, unicode.IsSpace) {
		return ""
	}
	if u, err := url.Parse(code); err == nil && u.Host != "" {
		return u.Host
	}
	host, rest, ok := strings.Cut(code, "/")
	if ok && rest != "" && strings.Contains(host, ".") {
		return host
	}
	return ""
}

// steamShaped reports the XXXXX-XXXXX-XXXXX form, which is the one code
// layout common enough here to be worth naming.
func steamShaped(code string) bool {
	groups := strings.Split(code, "-")
	if len(groups) != 3 {
		return false
	}
	for _, g := range groups {
		if len(g) != 5 {
			return false
		}
		for _, r := range g {
			if !isAlnum(r) {
				return false
			}
		}
	}
	return true
}

// classOf names the characters a single-token code is made of, which is as
// much as can be said about it without saying any of it.
func classOf(code string) string {
	var letters, digits, other bool
	for _, r := range code {
		switch {
		case unicode.IsLetter(r):
			letters = true
		case unicode.IsDigit(r):
			digits = true
		default:
			other = true
		}
	}
	var parts []string
	if letters {
		parts = append(parts, "letters")
	}
	if digits {
		parts = append(parts, "digits")
	}
	if other {
		parts = append(parts, "punctuation")
	}
	switch len(parts) {
	case 0:
		return "nothing readable"
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
