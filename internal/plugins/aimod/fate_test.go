package aimod

import (
	"testing"
)

// A jump link to a message merlin just deleted opens on "Unknown message", so
// the entry has to say it is gone before anybody clicks, and a rewrite has to
// link both halves: the deleted original and the repost the channel shows.
func TestMessageFateSaysWhatHappenedToTheMessage(t *testing.T) {
	c := candidate{ChannelID: "c", MessageID: "m"}
	orig := "https://discord.com/channels/g/c/m"
	repost := "https://discord.com/channels/g/c/r"
	cases := []struct {
		action, rewrite, repostID, want string
	}{
		{"aimod.flagged", "", "", "[Jump to message](" + orig + ")"},
		{"aimod.dryrun", "", "", "[Jump to message](" + orig + ")"},
		{"aimod.remove", "", "", "[Deleted](" + orig + ")"},
		{"aimod.rewrite", "cleaned", "r", "[Deleted](" + orig + ") → [Reposted](" + repost + ")"},
		// The repost landed but Discord did not say where: still say so.
		{"aimod.rewrite", "cleaned", "", "[Deleted](" + orig + ") → Reposted"},
		// Nothing publishable left means rewriteMessage removed it.
		{"aimod.rewrite", "  ", "", "[Deleted](" + orig + ")"},
	}
	for _, tc := range cases {
		got := messageFate("g", tc.action, c, deepVerdict{Rewrite: tc.rewrite}, tc.repostID)
		if got != tc.want {
			t.Errorf("%s/%q: got %q, want %q", tc.action, tc.repostID, got, tc.want)
		}
	}
}

// The rewrite path has to hand back where the repost landed, or the audit
// entry has nothing to link "Reposted" to.
func TestRewriteReturnsTheRepostID(t *testing.T) {
	p := testPlugin(t, newFakeStore(), &fakeClassifier{}, newFakeOps(), &fakeAudit{})
	id, err := p.rewriteMessage(t.Context(), "g1", candidate{ChannelID: "c1", MessageID: "m1", AuthorID: "u1"}, "fine words", "abcd2345")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if id != "repost1" {
		t.Errorf("repost ID = %q, want the webhook's message", id)
	}
}

func TestConfidenceMeterAndPolicyLabel(t *testing.T) {
	for in, want := range map[float64]string{
		0.95: "▰▰▰▰▱ 95%",
		0.84: "▰▰▰▰▱ 84%",
		0:    "▱▱▱▱▱ 0%",
		1.7:  "▰▰▰▰▰ 100%",
	} {
		if got := confidenceMeter(in); got != want {
			t.Errorf("confidenceMeter(%v) = %q, want %q", in, got, want)
		}
	}
	if got := policyLabel("hate_speech"); got != "Hate speech" {
		t.Errorf("policyLabel = %q", got)
	}
}
