package aimod

import (
	"strings"
	"testing"
)

// A jump link to a message merlin just deleted renders as "Unknown message",
// so the audit line has to say it is gone rather than leave a moderator to
// find out by clicking. The link itself stays, since it carries the ID.
func TestMessageFateSaysWhatHappenedToTheMessage(t *testing.T) {
	c := candidate{ChannelID: "c", MessageID: "m"}
	link := "https://discord.com/channels/g/c/m"
	cases := []struct {
		action, rewrite, want string
	}{
		{"aimod.flagged", "", "[Jump to message](" + link + ")"},
		{"aimod.dryrun", "", "[Jump to message](" + link + ")"},
		{"aimod.remove", "", "~~[message](" + link + ")~~ deleted"},
		{"aimod.rewrite", "cleaned", "~~[original](" + link + ")~~ deleted and reposted, rewritten"},
		// Nothing publishable left means rewriteMessage removed it.
		{"aimod.rewrite", "  ", "~~[message](" + link + ")~~ deleted"},
	}
	for _, tc := range cases {
		got := messageFate("g", tc.action, c, deepVerdict{Rewrite: tc.rewrite})
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.action, got, tc.want)
		}
		if !strings.Contains(got, "m)") {
			t.Errorf("%s: the message ID dropped out of the link: %q", tc.action, got)
		}
	}
}
