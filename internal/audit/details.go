package audit

import (
	"strconv"
	"strings"
)

// readable lays a "key=value" audit detail out as one labelled line per pair.
//
// Most call sites record details as `user=<@1> duration=10m reason="x"`,
// which is the right shape for the durable row (greppable, and every
// existing entry already has it) and the wrong one for a moderator reading
// the channel: it wrapped into a single run-on line with the quoting and the
// brackets left in. This is the render-time half of the same split
// FormatActor makes, the row stays machine-shaped and the embed reads as
// prose, so no call site and no stored row changes.
//
// It is deliberately forgiving: words that are not a pair stay as prose in
// their original order, a quoted value that does not unquote is left as
// words, and a value with no pairs at all, or one that already has its own
// line breaks, comes back untouched.
func readable(v string) string {
	if !strings.Contains(v, "=") || strings.Contains(v, "\n") {
		return v
	}
	var lines, prose []string
	flush := func() {
		if len(prose) > 0 {
			lines = append(lines, strings.Join(prose, " "))
			prose = nil
		}
	}
	found := false
	for rest := strings.TrimSpace(v); rest != ""; rest = strings.TrimLeft(rest, " ") {
		key, val, n, ok := pair(rest)
		if !ok {
			word, after, _ := strings.Cut(rest, " ")
			prose = append(prose, word)
			rest = after
			continue
		}
		found = true
		rest = rest[n:]
		if val == "" {
			// "role= user=<@1>": call sites with an either/or pair leave one
			// half empty, and a labelled blank reads as missing data.
			continue
		}
		flush()
		lines = append(lines, "**"+label(key)+"** "+val)
	}
	flush()
	if !found {
		return v
	}
	return strings.Join(lines, "\n")
}

// pair reads one key=value token off the front of s, returning how many bytes
// it consumed. Values are a Go-quoted string (what %q wrote), a bracketed
// list (what %v of a slice, or "[%s]" of MentionRoles, wrote), or a bare word.
func pair(s string) (key, val string, n int, ok bool) {
	i := 0
	for i < len(s) && (s[i] == '_' || s[i] >= 'a' && s[i] <= 'z') {
		i++
	}
	if i == 0 || i >= len(s) || s[i] != '=' {
		return "", "", 0, false
	}
	key, rest := s[:i], s[i+1:]
	switch {
	case strings.HasPrefix(rest, `"`):
		q, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return "", "", 0, false
		}
		val, _ = strconv.Unquote(q)
		return key, val, i + 1 + len(q), true
	case strings.HasPrefix(rest, "["):
		end := strings.IndexByte(rest, ']')
		if end < 0 {
			return "", "", 0, false
		}
		val = strings.TrimSpace(rest[1:end])
		if val == "" {
			// An empty list is a fact ("restored nothing"), unlike an empty
			// scalar, so it is said rather than dropped.
			val = "none"
		}
		return key, val, i + 1 + end + 1, true
	default:
		val, _, _ = strings.Cut(rest, " ")
		return key, val, i + 1 + len(val), true
	}
}

// labels covers the keys whose mechanical rendering would mislead or read
// badly. Everything else is de-underscored, the same fallback humanize gives
// actions.
var labels = map[string]string{
	"user":               "Member",
	"restored":           "Roles restored",
	"unmanageable_roles": "Kept (merlin can't remove)",
	"linked_to":          "Linked to",
}

func label(key string) string {
	if l, ok := labels[key]; ok {
		return l
	}
	return upperFirst(strings.ReplaceAll(key, "_", " "))
}
