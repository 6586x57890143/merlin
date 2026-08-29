package voice

// Key names one thing merlin can say. Adding a public-facing message means
// adding a Key here, a spec below, and a block of lines in lines/*.yaml.
// Anything missing from any of the three fails startup rather than shipping
// half-wired.
type Key string

const (
	// The six notices posted into a freshly rotated channel, one per
	// combination of the guild's chosen settings.Disclosure and whether it
	// keeps archives for a bounded window. rotation.introKey picks between
	// them; the required placeholders below are what stop a mode publishing
	// a fact the guild chose to withhold.
	//
	// KeyRotationIntroFull states the cadence and the deletion window.
	KeyRotationIntroFull Key = "rotation.intro.full"
	// KeyRotationIntroFullForever states the cadence where archives are kept
	// indefinitely, so there is no deletion window to state.
	KeyRotationIntroFullForever Key = "rotation.intro.full_forever"
	// KeyRotationIntroCadence states only how often the channel resets.
	KeyRotationIntroCadence Key = "rotation.intro.cadence"
	// KeyRotationIntroRetention states only how long the archive survives.
	KeyRotationIntroRetention Key = "rotation.intro.retention"
	// KeyRotationIntroRetentionForever is the retention-only notice where
	// archives are kept indefinitely: nothing is deleted, it moves out of
	// reach. No numbers to state, so no placeholders.
	KeyRotationIntroRetentionForever Key = "rotation.intro.retention_forever"
	// KeyRotationIntroGeneric states only that the channel rotated.
	KeyRotationIntroGeneric Key = "rotation.intro.generic"

	// KeyRotationHeadsUp warns the channel that is about to be rotated.
	KeyRotationHeadsUp Key = "rotation.headsup"
	// KeyRotationHeadsUpGeneric is the same warning without the countdown,
	// for a channel on generic disclosure: naming the remaining minutes
	// would hand over the rotation schedule the guild chose not to publish.
	KeyRotationHeadsUpGeneric Key = "rotation.headsup.generic"

	// KeyJailNotice and KeyReleaseNotice are DMs to the member concerned,
	// never channel posts.
	KeyJailNotice    Key = "moderation.jail"
	KeyReleaseNotice Key = "moderation.release"

	// KeyAIModRemoved and KeyAIModRewritten are DMs to the member whose
	// message the AI moderation plugin acted on. Plain register, like the
	// jail notices above and for the same reason, with one thing on top:
	// nobody decided this by hand. A member reading it has just had
	// something they wrote taken down by software, with no person in the
	// loop, and the line has to carry that honestly rather than pretend a
	// moderator was involved. The reason and the original text ride along as
	// embed fields, so the wording never has to carry them.
	KeyAIModRemoved   Key = "moderation.aimod_removed"
	KeyAIModRewritten Key = "moderation.aimod_rewritten"

	// KeyJailAnnounce and KeyReleaseAnnounce are posted publicly into the
	// channel a jail or release command was run in, read by whoever else is
	// sitting in that channel rather than the member concerned. That is a
	// different audience from moderation.jail/moderation.release above, so
	// they run in the ordinary playful register instead of the plain one:
	// see PERSONA.md's "Where the character stops" for why that split is
	// deliberate rather than an oversight.
	KeyJailAnnounce    Key = "roles.jail_announce"
	KeyReleaseAnnounce Key = "roles.release_announce"

	// The router's own refusals, which any member can trigger.
	KeyDenied         Key = "system.denied"
	KeyPluginDisabled Key = "system.plugin_disabled"
	KeyGuildOnly      Key = "system.guild_only"
	KeyStaleComponent Key = "system.stale_component"
	KeyBroken         Key = "system.broken"

	KeyPing Key = "system.ping"

	// The tip jar, shown by /aimod funding: a public surface, so the prose
	// around the numbers is hers. The wallet address, the balances and the
	// warning about who controls the wallet are deliberately NOT here. Line
	// selection is random and falls back silently, which is right for a
	// greeting and wrong for the one sentence telling somebody where their
	// money is about to go.
	KeyFundingAsk    Key = "funding.ask"
	KeyFundingThanks Key = "funding.thanks"
	KeyFundingLow    Key = "funding.low"
	KeyFundingDry    Key = "funding.dry"
)

// Register is how much personality a surface gets.
type Register int

const (
	// RegisterPlayful is merlin's ordinary voice: casual, lowercase, the
	// occasional emoji. Everything ambient that a member bumps into.
	RegisterPlayful Register = iota
	// RegisterPlain is for moderation outcomes. Still her, still warm, but
	// it explains rather than jokes. A punchline aimed at someone who has
	// just been punished is what turns a moderation action into a
	// screenshot, and the person reading it is having a bad minute already.
	RegisterPlain
)

// Discord's own ceilings, for reference. The per-key limits below are far
// lower on purpose: these messages are read in passing, and a wall of text
// in a chat channel is ignored no matter how well it is written.
const (
	maxMessageContent   = 2000
	maxEmbedDescription = 4096
)

// minLinesPerKey is how much variety a key has to carry before it counts as
// varied. Two lines alternating is arguably worse than one line repeating,
// because the alternation itself becomes the pattern people notice.
//
// It was four, which was enough to prove the mechanism and not enough to
// survive contact with a busy server. A channel on a six hour rotation burns
// through four intro lines in a day, and a refusal that any member can
// trigger by clicking the wrong button is repeated far more often than that.
// Eight is the floor; the keys people actually see hourly carry twelve to
// fourteen. Raising this is what turns "add a few more lines" into something
// the build checks rather than something that quietly rots.
const minLinesPerKey = 8

// spec is the contract a key's lines must satisfy, checked at startup.
type spec struct {
	register Register
	// required placeholders must appear in every single line. This is the
	// mechanism that keeps the retention disclosure from being written out
	// of existence by someone adding a line they thought was funnier.
	required []string
	// optional placeholders may appear. Anything outside required plus
	// optional is a typo ({cadance}) and fails the build.
	//
	// No spec sets this, and that emptiness is load bearing rather than an
	// oversight. It is what makes the rotation disclosure modes enforceable:
	// because a placeholder outside required-plus-optional is rejected, a
	// line that slipped {retention} into rotation.intro.cadence fails to
	// boot instead of publishing a deletion window the guild deliberately
	// withheld. Adding a placeholder here weakens that guarantee for the key
	// it is added to, so do it knowingly.
	optional []string
	maxLen   int
	// fallback is compiled in, and is what gets said if the catalog somehow
	// cannot render at runtime. It carries the same required placeholders,
	// so the guarantee survives even total catalog failure. Deliberately
	// the plainest possible wording: it exists to be correct, not good.
	fallback string
}

// specs is the whole contract, in one table, on purpose. Reading this
// should tell you everything merlin can say and what each line owes.
var specs = map[Key]spec{
	KeyRotationIntroFull: {
		register: RegisterPlayful,
		// Both of these are the actual published retention policy. spec.MD
		// section 6 step 7 exists to make this statement, and the code has
		// already got it wrong once by reporting the rotation cadence as
		// though it were the deletion window (see rotation/templates.go).
		// Requiring both in every line is what lets the wording vary freely
		// without the promise varying with it.
		required: []string{"cadence", "retention"},
		maxLen:   maxEmbedDescription,
		fallback: "this channel resets every {cadence}. anything archived is kept {retention} and then permanently deleted.",
	},
	KeyRotationIntroFullForever: {
		register: RegisterPlayful,
		required: []string{"cadence"},
		maxLen:   maxEmbedDescription,
		fallback: "this channel resets every {cadence}. the old channel is archived where only the moderators can reach it.",
	},
	// The narrower disclosure modes rely on the *other* half of the
	// placeholder contract: loadCatalog rejects any placeholder a key does
	// not require, so a line that slipped {retention} into a cadence-only
	// key fails the build rather than publishing a deletion window the guild
	// deliberately withheld. The omission is the feature here, so it is
	// enforced exactly as hard as the inclusion is above.
	KeyRotationIntroCadence: {
		register: RegisterPlayful,
		required: []string{"cadence"},
		maxLen:   maxEmbedDescription,
		fallback: "this channel resets every {cadence}.",
	},
	KeyRotationIntroRetention: {
		register: RegisterPlayful,
		required: []string{"retention"},
		maxLen:   maxEmbedDescription,
		fallback: "anything archived from this channel is kept {retention} and then permanently deleted.",
	},
	KeyRotationIntroRetentionForever: {
		register: RegisterPlayful,
		maxLen:   maxEmbedDescription,
		fallback: "the old channel is archived where only the moderators can reach it, and nothing on it is deleted.",
	},
	KeyRotationIntroGeneric: {
		register: RegisterPlayful,
		maxLen:   maxEmbedDescription,
		fallback: "this channel has rotated. this is the new one.",
	},

	KeyRotationHeadsUp: {
		register: RegisterPlayful,
		required: []string{"when"},
		// maxEmbedDescription rather than maxMessageContent: the heads-up is
		// posted as an embed description now, matching the intro notice it
		// precedes in the same channel.
		maxLen:   maxEmbedDescription,
		fallback: "heads up: this channel resets in {when}.",
	},
	KeyRotationHeadsUpGeneric: {
		register: RegisterPlayful,
		maxLen:   maxEmbedDescription,
		fallback: "heads up: this channel resets shortly.",
	},

	KeyJailNotice: {
		register: RegisterPlain,
		required: []string{"guild", "until"},
		// The reason, when a mod gave one, is attached as its own embed
		// field rather than substituted into the sentence. A placeholder
		// that is only sometimes supplied would make every line carrying it
		// fall back on the occasions it is missing, which is the worst of
		// both: the variety disappears exactly when the message matters
		// most, and nothing says why.
		maxLen:   maxEmbedDescription,
		fallback: "you have been jailed in {guild}. your roles are saved and come back {until}.",
	},
	KeyReleaseNotice: {
		register: RegisterPlain,
		required: []string{"guild"},
		maxLen:   maxEmbedDescription,
		fallback: "you are out. your roles in {guild} have been restored.",
	},

	KeyAIModRemoved: {
		register: RegisterPlain,
		required: []string{"guild"},
		maxLen:   maxEmbedDescription,
		fallback: "a message you posted in {guild} was removed automatically, because it matched one of Discord's own rules. no moderator saw it first.",
	},
	KeyAIModRewritten: {
		register: RegisterPlain,
		required: []string{"guild"},
		maxLen:   maxEmbedDescription,
		fallback: "part of a message you posted in {guild} was edited out automatically, because it matched one of Discord's own rules. the rest of it is still there.",
	},

	KeyJailAnnounce: {
		register: RegisterPlayful,
		// until, not duration: matching KeyJailNotice's own {until}, this is
		// Discord's own relative timestamp markup so the channel post and
		// the DM describe the same moment the same way, each reader seeing
		// it rendered in their own timezone.
		required: []string{"members", "until"},
		// maxMessageContent, not maxEmbedDescription: this is posted as
		// plain message content (announce.go), not an embed, so the real
		// ceiling is Discord's 2000-byte message limit, not the 4096-byte
		// embed description one.
		maxLen:   maxMessageContent,
		fallback: "{members} has been jailed. back {until}.",
	},
	KeyReleaseAnnounce: {
		register: RegisterPlayful,
		required: []string{"members"},
		maxLen:   maxMessageContent,
		fallback: "{members} has been released.",
	},

	KeyDenied: {
		register: RegisterPlayful,
		maxLen:   maxMessageContent,
		fallback: "you are not allowed to run this command.",
	},
	KeyPluginDisabled: {
		register: RegisterPlayful,
		maxLen:   maxMessageContent,
		fallback: "that feature is switched off in this server.",
	},
	KeyGuildOnly: {
		register: RegisterPlayful,
		maxLen:   maxMessageContent,
		fallback: "that only works inside a server.",
	},
	KeyStaleComponent: {
		register: RegisterPlayful,
		maxLen:   maxMessageContent,
		fallback: "that button is not wired up any more.",
	},
	KeyBroken: {
		register: RegisterPlayful,
		maxLen:   maxMessageContent,
		fallback: "something went wrong handling that.",
	},
	KeyPing: {
		register: RegisterPlayful,
		maxLen:   maxMessageContent,
		fallback: "kik-ong!",
	},

	KeyFundingAsk: {
		register: RegisterPlayful,
		maxLen:   maxEmbedDescription,
		fallback: "this filter runs on donated credit. the jar is below.",
	},
	KeyFundingThanks: {
		register: RegisterPlayful,
		required: []string{"raised"},
		maxLen:   maxEmbedDescription,
		fallback: "{raised} donated so far. thank you.",
	},
	// Plain, for the same reason moderation.* is: a server about to lose its
	// filter is not a playful moment. {runway} is required because how long
	// is left is the entire content of the warning, and a line that dropped
	// it would read as a vague grumble rather than a deadline.
	KeyFundingLow: {
		register: RegisterPlain,
		required: []string{"runway"},
		maxLen:   maxEmbedDescription,
		fallback: "about {runway} of scanning credit left.",
	},
	KeyFundingDry: {
		register: RegisterPlain,
		maxLen:   maxEmbedDescription,
		fallback: "the scanning credit has run out. only the built-in pattern checks are running.",
	},
}
