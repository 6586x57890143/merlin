package audit

import (
	"strings"
	"unicode"

	"github.com/6586x57890143/merlin/internal/core"

	"github.com/bwmarrin/discordgo"
)

// meta is how one audit action is presented: what it is called in the embed
// title, and how severe it is.
type meta struct {
	title string
	color int
	// mood overrides the colour-derived face. Only set where the palette
	// cannot express the distinction, matching core.WithMood's own rule.
	mood core.Mood
}

// actions is the presentation table.
//
// It exists because the embed used to title itself with the raw action
// string, so a moderator scrolling the audit channel read
// "config.permission_tier_set" and "rotation.channel_deleted" as equally
// weighted lines of code. The colour was ColorSuccess for every single entry,
// including permanent channel deletion, so the one visual cue the format has
// was actively misleading.
//
// A table can drift from the call sites; a purely mechanical de-dot would
// never drift but reads worse everywhere. This takes the table and backs it
// with the mechanical version (humanize below), so a missing entry degrades
// to something readable rather than to something wrong or blank. That makes
// an unlisted action a polish gap noticed at leisure instead of a bug, which
// is the right severity: nothing here changes what is stored, only how it is
// shown.
var actions = map[string]meta{
	// Irreversible. These are the entries somebody goes looking for after
	// content is gone, and they should not look like routine bookkeeping.
	"archive.deleted":          {title: "Archive permanently deleted", color: core.ColorError},
	"rotation.channel_deleted": {title: "Rotating channel deleted", color: core.ColorError},

	// Restrictive or removing.
	"roles.jail":                {title: "Member jailed", color: core.ColorWarning},
	"roles.jail_bulk":           {title: "Members jailed", color: core.ColorWarning},
	"roles.jail_reapplied":      {title: "Jail re-applied after a rejoin", color: core.ColorWarning},
	"roles.jail_reasserted":     {title: "Jail reasserted (roles regranted)", color: core.ColorWarning},
	"rotation.remove":           {title: "Channel rotation removed", color: core.ColorWarning},
	"config.admin_removed":      {title: "Admin removed", color: core.ColorWarning},
	"config.mod_role_removed":   {title: "Mod role removed", color: core.ColorWarning},
	"config.permission_revoked": {title: "Permission grant revoked", color: core.ColorWarning},
	"config.permission_blocked": {title: "Permission denied", color: core.ColorWarning},
	"config.writes_paused":      {title: "Write pause changed", color: core.ColorWarning},

	// Granting or restoring.
	"roles.release":                 {title: "Member released", color: core.ColorSuccess},
	"roles.grant":                   {title: "Role granted", color: core.ColorSuccess},
	"config.admin_added":            {title: "Admin added", color: core.ColorSuccess},
	"config.mod_role_added":         {title: "Mod role added", color: core.ColorSuccess},
	"config.permission_granted":     {title: "Permission granted", color: core.ColorSuccess},
	"config.permission_unblocked":   {title: "Permission un-denied", color: core.ColorSuccess},
	"config.setup":                  {title: "Setup step completed", color: core.ColorSuccess},
	"config.plugin_enabled":         {title: "Plugin enabled", color: core.ColorSuccess},
	"rotation.add":                  {title: "Channel rotation configured", color: core.ColorSuccess},
	"rotation.archive_role_allowed": {title: "Archive access granted to role", color: core.ColorSuccess},

	// Routine.
	"channel.rotated":               {title: "Channel rotated", color: core.ColorInfo},
	"rotation.edit":                 {title: "Rotation settings changed", color: core.ColorInfo},
	"rotation.sticky":               {title: "Sticky messages changed", color: core.ColorInfo},
	"rotation.archive_role_removed": {title: "Archive access removed from role", color: core.ColorWarning},
	// Drift correction, recorded only when the periodic check actually had to
	// change something. Info, not warning: an admin adding a role to the
	// archive category by hand is a normal thing to do, and this line is how
	// they find out the bot took it back off again.
	"rotation.archive_perms_fixed":   {title: "Archive permissions resynced", color: core.ColorInfo},
	"config.imported":                {title: "Legacy config imported", color: core.ColorInfo},
	"config.permission_tier_set":     {title: "Required tier changed", color: core.ColorInfo},
	"config.permission_tier_cleared": {title: "Required tier reset", color: core.ColorInfo},
	"config.plugin_disabled":         {title: "Plugin disabled", color: core.ColorWarning},
	"roles.configure_jail_channels":  {title: "Jail channel allowlist changed", color: core.ColorInfo},

	// Dry run. Reported as warnings so they stand out from real actions, but
	// wearing the idle face rather than the alarmed one: a bot in dry-run is
	// doing exactly what it was told. Same reasoning, and the same mechanism,
	// as /config pause and /config dryrun.
	"rotation.dryrun":       {title: "Rotation skipped (dry run)", color: core.ColorWarning, mood: core.MoodIdle},
	"archive.dryrun":        {title: "Archive deletion skipped (dry run)", color: core.ColorWarning, mood: core.MoodIdle},
	"config.writes_dry_run": {title: "Dry-run mode changed", color: core.ColorWarning, mood: core.MoodIdle},
}

// metaFor returns how to present action, falling back to a mechanical
// rendering for anything the table has not been told about.
//
// The unlisted default is ColorInfo on purpose. Claiming success for
// something the code does not recognise would be a lie in the direction that
// matters least visibly, and claiming an error would cry wolf over what is
// most likely a benign new config action. Info is the honest middle, and it
// is also a quiet signal that the table wants a row.
func metaFor(action string) meta {
	if m, ok := actions[action]; ok {
		return m
	}
	return meta{title: humanize(action), color: core.ColorInfo}
}

// humanize turns a dotted action key into something readable:
// "config.mod_role_added" becomes "Config: mod role added".
//
// It keeps the namespace rather than dropping to the last segment, because
// "Added" on its own is genuinely ambiguous in a channel carrying config,
// rotation and roles entries side by side.
//
// It never returns an empty string for a non-empty input, which is the
// property that matters: the title is the only thing distinguishing one audit
// entry from another, and a blank one would be strictly worse than the raw
// dotted key this replaced.
func humanize(action string) string {
	if action == "" {
		return "Audit entry"
	}
	namespace, rest, found := strings.Cut(action, ".")
	if !found {
		return upperFirst(strings.ReplaceAll(namespace, "_", " "))
	}
	rest = strings.ReplaceAll(strings.ReplaceAll(rest, ".", " "), "_", " ")
	if rest == "" {
		return upperFirst(namespace)
	}
	return upperFirst(namespace) + ": " + rest
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// buildEmbed renders one audit entry.
//
// Pure, and separate from Record's database write and Discord send, because
// Writer holds a concrete *discordgo.Session and so nothing in this package
// was testable at all. Titles, colours, actor rendering and truncation are
// the parts most likely to be got wrong and the parts most worth asserting.
func buildEmbed(actorID, action, oldValue, newValue string) *discordgo.MessageEmbed {
	m := metaFor(action)
	embed := core.NewEmbed(m.color, m.title, "")
	if m.mood != core.MoodNone {
		embed = core.WithMood(embed, m.mood)
	}

	embed.Fields = []*discordgo.MessageEmbedField{
		{Name: "Actor", Value: core.FormatActor(actorID), Inline: true},
	}

	// Truncated because an embed field over 1024 bytes doesn't get trimmed by
	// Discord, it fails the entire message, so a single long value (a sweep
	// listing many channel IDs, a rotation config with a long sticky) would
	// silently cost the guild its live audit notification for that action.
	// The durable row is already written in full and is unaffected.
	switch {
	case oldValue != "" && newValue != "":
		embed.Fields = append(embed.Fields,
			&discordgo.MessageEmbedField{Name: "Before", Value: core.TruncateEmbedField(oldValue), Inline: true},
			&discordgo.MessageEmbedField{Name: "After", Value: core.TruncateEmbedField(newValue), Inline: true},
		)
	case newValue != "":
		// Not inline: with nothing to sit beside, a half-width field just
		// leaves a gap, and these carry the detail worth reading.
		embed.Fields = append(embed.Fields,
			&discordgo.MessageEmbedField{Name: "Details", Value: core.TruncateEmbedField(newValue)})
	case oldValue != "":
		embed.Fields = append(embed.Fields,
			&discordgo.MessageEmbedField{Name: "Removed", Value: core.TruncateEmbedField(oldValue)})
	}
	return embed
}
