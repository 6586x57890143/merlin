package dragoman

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// carriers is everything in a message that is not language: code, links,
// mentions, custom emoji, timestamps. Each goes to the model as a numbered
// token and comes back as itself, so a translation can neither mangle one
// nor bring in one the author didn't write, and no ID is ever sent.
//
// Code is only a block that names its language. Members put whole messages
// in backticks for the look of it, and an untagged block or `span` is
// translated like the words around it, backticks kept.
var carriers = regexp.MustCompile("(?is)```[a-z0-9+#-]+\n.*?```" +
	`|<(?:@[!&]?|#)\d+>|<a?:\w+:\d+>|<t:-?\d+(?::\w)?>|</[^:>\n]+:\d+>|<id:\w+>|@(?:everyone|here)\b` +
	`|<?\b[a-z][a-z0-9+.-]*://[^\s>]+>?|\bdiscord(?:app)?\.(?:gg|com)/\S+` +
	`|\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:com|gg|ru|xyz|link|io|net|org|me|co|uk|nl|de|dev|app|info|top|site|shop|click)\b(?:/\S*)?`)

const (
	tokenOpen  = "⟦"
	tokenClose = "⟧"
)

var token = regexp.MustCompile(tokenOpen + `(\d+)` + tokenClose)

// mask replaces text's carriers with tokens. ok is false for a text that
// already holds something shaped like one.
func mask(text string) (masked string, parts []string, ok bool) {
	if strings.Contains(text, tokenOpen) || strings.Contains(text, tokenClose) {
		return "", nil, false
	}
	masked = carriers.ReplaceAllStringFunc(text, func(m string) string {
		parts = append(parts, m)
		return fmt.Sprintf("%s%d%s", tokenOpen, len(parts), tokenClose)
	})
	return masked, parts, true
}

// unmask puts the carriers back. ok is false unless every token is there
// exactly once and nothing that should have been one was added.
func unmask(text string, parts []string) (out string, ok bool) {
	seen := make([]bool, len(parts))
	ok = true
	bare := token.ReplaceAllString(text, " ")
	if carriers.MatchString(bare) || strings.Contains(bare, tokenOpen) || strings.Contains(bare, tokenClose) {
		return "", false
	}
	out = token.ReplaceAllStringFunc(text, func(m string) string {
		n, err := strconv.Atoi(token.FindStringSubmatch(m)[1])
		if err != nil || n < 1 || n > len(parts) || seen[n-1] {
			ok = false
			return ""
		}
		seen[n-1] = true
		return parts[n-1]
	})
	for _, s := range seen {
		ok = ok && s
	}
	return out, ok
}
