package audit

import (
	"strconv"
	"strings"
)

// item is one piece of a key=value detail: a pair, or (key "") a run of
// prose words kept in its original position.
type item struct{ key, val string }

// parseDetail splits a "key=value" audit detail into items, or reports
// ok=false when v is not in that format at all.
//
// Most call sites record details as `user=<@1> duration=10m reason="x"`,
// which is the right shape for the durable row (greppable, and every
// existing entry already has it) and the wrong one for a moderator reading
// the channel: it wrapped into a single run-on line with the quoting and the
// brackets left in. toEntry parses it into the ledger entry's fields, the
// same split FormatActor makes: the strings stay as written in the row and
// the card reads as fields.
//
// It is deliberately forgiving: words that are not a pair stay as prose in
// their original order, a quoted value that does not unquote is left as
// words, and a value with no pairs at all, or one that already has its own
// line breaks, is not this format.
func parseDetail(v string) (items []item, ok bool) {
	if !strings.Contains(v, "=") || strings.Contains(v, "\n") {
		return nil, false
	}
	var prose []string
	flush := func() {
		if len(prose) > 0 {
			items = append(items, item{val: strings.Join(prose, " ")})
			prose = nil
		}
	}
	for rest := strings.TrimSpace(v); rest != ""; rest = strings.TrimLeft(rest, " ") {
		key, val, n, isPair := pair(rest)
		if !isPair {
			word, after, _ := strings.Cut(rest, " ")
			prose = append(prose, word)
			rest = after
			continue
		}
		ok = true
		rest = rest[n:]
		if val == "" {
			// "role= user=<@1>": call sites with an either/or pair leave one
			// half empty, and a labelled blank reads as missing data.
			continue
		}
		flush()
		items = append(items, item{key, val})
	}
	flush()
	return items, ok
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
