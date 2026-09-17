package rapsheet

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/core"
)

// Rendering a sheet.
//
// Two audiences read the same record. A moderator gets everything: score,
// ladder position, linked accounts, hints, who recorded what. The member
// themselves (/rapsheet me) gets the entries that are about things they
// experienced, with the moderators anonymised and staff-internal notes
// left out. Both come out of renderSheet, switched on forMember, so the
// two views cannot drift on what an entry says.

// viewPrefix namespaces the mod view's pagination buttons; the user id
// rides in the CustomID so a click re-derives the sheet with no session.
const (
	viewPrefix = "rapsheet:view:"
	mePrefix   = "rapsheet:me:page:"
	pageSep    = ":page:"
)

func viewPagePrefix(userID string) string { return viewPrefix + userID + pageSep }

// parseViewCustomID recovers (userID, page) from a mod-view button.
func parseViewCustomID(customID string) (string, int, error) {
	rest := strings.TrimPrefix(customID, viewPrefix)
	userID, pageStr, ok := strings.Cut(rest, pageSep)
	if !ok || userID == "" {
		return "", 0, fmt.Errorf("rapsheet: malformed view custom id %q", customID)
	}
	page, err := core.ParsePaginationPage(pageStr, "")
	if err != nil {
		return "", 0, err
	}
	return userID, page, nil
}

// sheetView is everything renderSheet needs beyond the sheet itself.
type sheetView struct {
	Sheet     sheet
	Config    Config
	UserID    string
	Name      string
	Now       time.Time
	Page      int
	ForMember bool
	Hints     []AltHint
}

func renderSheet(v sheetView) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	entries := v.Sheet.Entries
	if v.ForMember {
		entries = memberVisible(entries)
	}
	pageEntries, page, totalPages := paginate(entries, v.Page)

	var b strings.Builder
	writeHeader(&b, v)
	if len(entries) == 0 {
		b.WriteString("\nNothing on record.")
	} else {
		b.WriteString("\n")
		for _, e := range pageEntries {
			b.WriteString(entryLine(e, v.Now, v.Config.HalfLife, v.ForMember))
			b.WriteString("\n")
		}
	}

	title := "Rapsheet: " + v.Name
	if v.ForMember {
		title = "Your rapsheet"
	}
	color := core.ColorInfo
	if _, ok := standing(v.Sheet.Entries, v.Now); ok {
		color = core.ColorWarning
	}
	embed := core.NewEmbed(color, title, core.TruncateEmbedDescription(strings.TrimRight(b.String(), "\n")))

	prefix := viewPagePrefix(v.UserID)
	if v.ForMember {
		prefix = mePrefix
	}
	return embed, core.PaginationRow(prefix, page, totalPages)
}

// writeHeader is the summary block above the entries.
func writeHeader(b *strings.Builder, v sheetView) {
	fmt.Fprintf(b, "**Score:** %.0f", v.Sheet.Score)
	if !v.ForMember {
		if v.Sheet.Rec.Action != ActionNone {
			fmt.Fprintf(b, " · **Band:** %s", recWords(v.Sheet.Rec))
		}
		fmt.Fprintf(b, " · %d entries · half-life %s", len(v.Sheet.Entries), core.FormatDuration(v.Config.HalfLife))
	}
	b.WriteString("\n")
	if v.ForMember && v.Sheet.Rec.Action != ActionNone {
		fmt.Fprintf(b, "Your record is in this server's **%s** band. Points fade with time; each line says what it still counts for.\n", recWords(v.Sheet.Rec))
	}

	if st, ok := standing(v.Sheet.Entries, v.Now); ok {
		fmt.Fprintf(b, "**Standing:** %s", standingWords(st))
		if !v.ForMember {
			fmt.Fprintf(b, " (#%d)", st.ID)
		}
		b.WriteString("\n")
	}

	if v.ForMember {
		return
	}
	if len(v.Sheet.Group) > 1 {
		var others []string
		for _, id := range v.Sheet.Group {
			if id != v.UserID {
				others = append(others, core.MentionUser(id))
			}
		}
		fmt.Fprintf(b, "**Linked accounts:** %s (one shared score)\n", strings.Join(others, ", "))
	}
	if len(v.Hints) > 0 {
		var parts []string
		for _, h := range v.Hints {
			other := h.CandidateID
			if other == v.UserID {
				other = h.UserID
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", core.MentionUser(other), strings.Join(h.Signals, ", ")))
		}
		fmt.Fprintf(b, "**Possible alts:** %s. Confirm with `/rapsheet link`.\n", strings.Join(parts, "; "))
	}
}

// memberVisible is the subset of a sheet its subject may see: what was
// done to them, never staff's notes to each other or the ladder's own
// bookkeeping, and never a voided entry, because a voided entry is a
// decision that was taken back and there is nothing for them to act on.
func memberVisible(entries []Entry) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Voided() {
			continue
		}
		switch e.Kind {
		case KindNote, KindSuggestion:
			continue
		}
		out = append(out, e)
	}
	return out
}

// entryLine is one entry as one or two lines of the description.
func entryLine(e Entry, now time.Time, halfLife time.Duration, forMember bool) string {
	var b strings.Builder
	head := fmt.Sprintf("#%d %s", e.ID, kindWords(e))
	if e.Voided() {
		head = "~~" + head + "~~"
	}
	b.WriteString("**" + head + "**")
	// The category is the offence's, so it rides only on a scored entry. A
	// consequence (a jail, a ban) or a ladder row has no offence of its
	// own, and "jailed 8h · server rule" over an aimod hate-speech reason
	// read as a contradiction.
	if e.Points > 0 {
		b.WriteString(" · " + categoryLabel(e.Category))
		if e.Voided() {
			fmt.Fprintf(&b, " · %d pts (voided)", e.Points)
		} else {
			fmt.Fprintf(&b, " · %d pts (%.0f now)", e.Points, Decayed(e.Points, now.Sub(e.CreatedAt), halfLife))
		}
	}
	b.WriteString(" · " + relativeTimestamp(e.CreatedAt))
	b.WriteString(" · by " + actorWords(e.ActorID, forMember))
	if e.Reason != "" {
		b.WriteString("\n> " + clip(oneLine(e.Reason), maxReasonShown))
	}
	if e.Voided() && !forMember {
		fmt.Fprintf(&b, "\n> voided by %s", actorWords(e.VoidedBy, false))
		if e.VoidReason != "" {
			b.WriteString(": " + clip(oneLine(e.VoidReason), maxVoidReasonShown))
		}
	}
	return b.String()
}

// The sheet is one embed description, capped by Discord at 4096 bytes, and
// a reason is free text a moderator typed. sheetPageSize entries with every
// reason at maxReasonShown and every void at maxVoidReasonShown fit under
// the cap with room for the header; TruncateEmbedDescription is only the
// backstop.
const (
	sheetPageSize      = 8
	maxReasonShown     = 150
	maxVoidReasonShown = 100
)

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// paginate is core.Paginate at the sheet's own page size.
func paginate(items []Entry, page int) (pageItems []Entry, clampedPage, totalPages int) {
	totalPages = max(1, (len(items)+sheetPageSize-1)/sheetPageSize)
	page = max(0, min(page, totalPages-1))
	start := page * sheetPageSize
	end := min(start+sheetPageSize, len(items))
	if start >= len(items) {
		return nil, page, totalPages
	}
	return items[start:end], page, totalPages
}

// kindWords is the entry's kind with its sentence, where it has one.
func kindWords(e Entry) string {
	switch e.Kind {
	case KindBan:
		if e.EndsAt == nil {
			return "banned permanently"
		}
		s := "banned " + core.FormatDuration(e.Duration)
		if e.LiftedAt != nil {
			s += " (lifted)"
		}
		return s
	case KindJail:
		return "jailed " + core.FormatDuration(e.Duration)
	case KindTimeout:
		return "timed out " + core.FormatDuration(e.Duration)
	case KindKick:
		return "kicked"
	case KindWarn:
		return "warned"
	case KindNote:
		return "note"
	case KindRemoval:
		return "message removed"
	case KindUnban:
		return "unbanned"
	case KindRelease:
		return "released"
	case KindSuggestion:
		return "ladder suggested"
	}
	return string(e.Kind)
}

// actorWords names who did it. A member reading their own sheet sees "a
// moderator": every line is about something they already experienced, and
// what the anonymity protects is the moderator from being singled out.
func actorWords(actorID string, forMember bool) string {
	if actorID == core.ActorSystem || actorID == "" {
		return "merlin (automatic)"
	}
	if forMember {
		return "a moderator"
	}
	return core.MentionUser(actorID)
}

func recWords(r Recommendation) string {
	switch r.Action {
	case ActionNone:
		return "nothing owed"
	case ActionNotice:
		return "notice"
	}
	return fmt.Sprintf("%s %s", r.Action, core.FormatDuration(r.Duration))
}

func standingWords(e Entry) string {
	switch e.Kind {
	case KindBan:
		if e.EndsAt == nil {
			return "banned permanently"
		}
		return "banned until " + absoluteTimestamp(*e.EndsAt)
	case KindJail:
		return "jailed until " + absoluteTimestamp(*e.EndsAt)
	case KindTimeout:
		return "timed out until " + absoluteTimestamp(*e.EndsAt)
	}
	return string(e.Kind)
}

// oneLine keeps a reason on the line the entry rendered it on. Reasons
// are typed into a slash-command option and cannot carry a newline, but a
// reason ingested from an aimod incident or a Discord audit entry can.
func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", " "))
}
