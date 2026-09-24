package audit

import (
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
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
// brackets left in. Parsing it at render time is the same split FormatActor
// makes: the row stays machine-shaped and the embed reads as a layout, so no
// stored row changes.
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

// readable is parseDetail flattened to one labelled line per pair, for the
// Before/After columns, where there is no room for a grid.
func readable(v string) string {
	items, ok := parseDetail(v)
	if !ok {
		return v
	}
	lines := make([]string, 0, len(items))
	for _, it := range items {
		if it.key == "" {
			lines = append(lines, it.val)
		} else {
			lines = append(lines, "**"+label(it.key)+"** "+it.val)
		}
	}
	return strings.Join(lines, "\n")
}

// maxColumnValue is the longest value that goes in a grid column. Past it a
// value wraps into a tall narrow strip beside short neighbours, so it gets
// the full width instead.
const maxColumnValue = 80

// gridLayout turns a detail into the embed's description and field grid.
//
// The reason is the sentence a moderator actually reads, so it leads, as a
// quote in the description beside the mood icon; prose goes there too. Every
// other pair becomes an inline field after the actor, which Discord lays out
// three to a row, so the entry reads as a table rather than a paragraph.
func gridLayout(items []item) (description string, fields []*discordgo.MessageEmbedField) {
	var desc []string
	for _, it := range items {
		switch it.key {
		case "":
			desc = append(desc, it.val)
		case "reason":
			desc = append(desc, "> "+strings.ReplaceAll(it.val, "\n", "\n> "))
		default:
			fields = append(fields, &discordgo.MessageEmbedField{
				Name:   label(it.key),
				Value:  core.TruncateEmbedField(it.val),
				Inline: len(it.val) <= maxColumnValue,
			})
		}
	}
	return core.TruncateEmbedDescription(strings.Join(desc, "\n")), fields
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
