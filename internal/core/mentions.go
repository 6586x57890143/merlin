package core

// Discord mention formatting, shared by every surface that reports which
// user, channel, or role something happened to.
//
// These exist because the audit trail was written for a database rather than
// a person: most call sites stored bare snowflakes ("actor=418...", "action=x
// role=530..."), a couple stored channel mentions, and the resulting embed was
// a wall of numbers that nobody could read without going and looking each one
// up. Formatting at the call site rather than at render time is deliberate:
// it means the durable audit_log row is readable too, which is the copy that
// outlives the channel, and it keeps the AuditWriter interface unchanged.
//
// A mention still contains the raw snowflake, so a later audit search or
// export can match on the ID exactly as it could before.
//
// None of this can cause a ping. Mentions inside an embed never notify
// anyone, discordguard zeroes AllowedMentions on every send it makes, and the
// two senders that bypass the guard (audit, scheduler alerts) zero it
// explicitly for exactly this reason.

// ActorSystem is the actor recorded for anything merlin did on her own: a
// scheduled rotation, a sweep, an automatic jail release.
//
// It is a sentinel rather than an empty string because "nobody told me to do
// this" and "I could not work out who told me to do this" are different
// facts, and the audit trail is the one place that distinction is worth
// keeping. FormatActor is what turns it back into something a human reads.
const ActorSystem = "system"

// MentionUser renders id as a user mention. An empty id yields an empty
// string rather than "<@>", which Discord renders as literal garbage; call
// sites that build "role=%s user=%s" style summaries routinely have one of
// the two empty and are expected to skip the empty half.
func MentionUser(id string) string {
	if id == "" {
		return ""
	}
	return "<@" + id + ">"
}

// MentionChannel renders id as a channel link. Empty in, empty out.
func MentionChannel(id string) string {
	if id == "" {
		return ""
	}
	return "<#" + id + ">"
}

// MentionRole renders id as a role mention. Empty in, empty out.
func MentionRole(id string) string {
	if id == "" {
		return ""
	}
	return "<@&" + id + ">"
}

// FormatActor renders an audit actor for a human reader.
//
// The ActorSystem case is the point of this function. Roughly half of all
// audit entries are automated (rotations, sweeps, jail releases, evasion
// re-applies) and every one of them recorded the bare word "system", which
// tells a moderator reading the channel nothing about who or what that is.
// Naming merlin explicitly is the difference between "system deleted a
// channel" and "the bot did this on schedule, not a person". The stored row
// keeps the sentinel; only the display changes.
func FormatActor(actorID string) string {
	switch actorID {
	case ActorSystem:
		return "merlin (automatic)"
	case "":
		// Reachable: adminconfig's audit helper substitutes an empty actor
		// when an interaction arrives without a Member, which happens for a
		// DM-context interaction. Better to say so than to render nothing
		// and leave the field looking broken.
		return "unknown"
	default:
		return MentionUser(actorID)
	}
}
