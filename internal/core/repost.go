package core

import "time"

// Prompt is one structured model call a plugin asks aimod to make on the
// guild's key and budget. Here rather than in aimod so a caller needs no
// import of that package: it takes this in its own narrow interface, which
// *aimod.Plugin satisfies.
type Prompt struct {
	System, User string
	// SchemaName and Schema hold the answer to a JSON Schema.
	SchemaName string
	Schema     map[string]any
	MaxTokens  int
	Timeout    time.Duration
	// Reserve refuses the call once the day's spend reaches this share of
	// the cap, leaving the rest to moderation. Zero reserves nothing.
	Reserve float64
	// Models are OpenRouter model IDs to use in place of the guild's deep
	// stack, in order. They need an OpenRouter key and are pinned to
	// endpoints that keep nothing, whichever gateway scans.
	Models []string
	// Brief asks a model that can't help thinking to think little. Thinking
	// is billed, and some default to as much of it as they can.
	Brief bool
}

// Answer is what a Prompt came back with and what it cost, in dollars. A
// failed call's cost is in it too.
type Answer struct {
	Text string
	Cost float64
}

// ModelPrice is dollars per million tokens.
type ModelPrice struct{ Prompt, Completion float64 }

// Repost is a member's message to be put back under their own name with
// different words.
type Repost struct {
	GuildID, ChannelID, MessageID, AuthorID string
	// RoleIDs and ReplyTo are the author's roles and the text the message
	// replied to, which moderating the repost reads.
	RoleIDs []string
	ReplyTo string
	// Original is the text Text was made from. A message that no longer says
	// it is left alone.
	Original string
	Text     string
	// Marker is appended to what is posted and is no part of what is judged.
	Marker string
	// Policy is the policy area Original breaks, where Text is a harmless
	// version of it rather than a faithful one. Empty when nothing was
	// softened. It is a hint: the original is judged on its own words.
	Policy string
	// Keep leaves the original up and posts beneath it, for a message whose
	// attachments a delete would destroy.
	Keep bool
}
