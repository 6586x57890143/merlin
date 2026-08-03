package voice

// Key names one thing Merlin can say. Adding a public-facing message means
// adding a Key here, a spec below, and a block of lines in lines/*.yaml.
// Anything missing from any of the three fails startup rather than shipping
// half-wired.
type Key string

const (
	// KeyRotationIntroKept is the notice posted into a freshly rotated
	// channel when the guild keeps archives for a bounded window.
	KeyRotationIntroKept Key = "rotation.intro.kept"
	// KeyRotationIntroForever is the same notice where archives are kept
	// indefinitely, so there is no deletion window to state.
	KeyRotationIntroForever Key = "rotation.intro.forever"
	// KeyRotationHeadsUp warns the channel that is about to be rotated.
	KeyRotationHeadsUp Key = "rotation.headsup"

	// KeyJailNotice and KeyReleaseNotice are DMs to the member concerned,
	// never channel posts.
	KeyJailNotice    Key = "moderation.jail"
	KeyReleaseNotice Key = "moderation.release"

	// The router's own refusals, which any member can trigger.
	KeyDenied         Key = "system.denied"
	KeyPluginDisabled Key = "system.plugin_disabled"
	KeyGuildOnly      Key = "system.guild_only"
	KeyStaleComponent Key = "system.stale_component"
	KeyBroken         Key = "system.broken"

	KeyPing Key = "system.ping"
)

// Register is how much personality a surface gets.
type Register int

const (
	// RegisterPlayful is Merlin's ordinary voice: casual, lowercase, the
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
const minLinesPerKey = 4

// spec is the contract a key's lines must satisfy, checked at startup.
type spec struct {
	register Register
	// required placeholders must appear in every single line. This is the
	// mechanism that keeps the retention disclosure from being written out
	// of existence by someone adding a line they thought was funnier.
	required []string
	// optional placeholders may appear. Anything outside required plus
	// optional is a typo ({cadance}) and fails the build.
	optional []string
	maxLen   int
	// fallback is compiled in, and is what gets said if the catalog somehow
	// cannot render at runtime. It carries the same required placeholders,
	// so the guarantee survives even total catalog failure. Deliberately
	// the plainest possible wording: it exists to be correct, not good.
	fallback string
}

// specs is the whole contract, in one table, on purpose. Reading this
// should tell you everything Merlin can say and what each line owes.
var specs = map[Key]spec{
	KeyRotationIntroKept: {
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
	KeyRotationIntroForever: {
		register: RegisterPlayful,
		required: []string{"cadence"},
		maxLen:   maxEmbedDescription,
		fallback: "this channel resets every {cadence}. the old channel is archived where only the moderators can reach it.",
	},
	KeyRotationHeadsUp: {
		register: RegisterPlayful,
		required: []string{"when"},
		maxLen:   maxMessageContent,
		fallback: "heads up: this channel resets in {when}.",
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
		maxLen: maxEmbedDescription,
		fallback: "you have been jailed in {guild}. your roles are saved and come back {until}.",
	},
	KeyReleaseNotice: {
		register: RegisterPlain,
		required: []string{"guild"},
		maxLen:   maxEmbedDescription,
		fallback: "you are out. your roles in {guild} have been restored.",
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
}
