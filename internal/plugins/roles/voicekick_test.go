package roles

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// Every path that (re)applies a jail's role set must also force-disconnect
// the member from voice: role/permission-overwrite changes don't propagate
// to an already-established voice session, so a jailed member who was
// mid-call would otherwise stay connected until they left on their own.

// applyJail is the initial /roles jail path.
func TestApplyJailDisconnectsFromVoice(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302"})

	p := newEvasionPlugin(t, ops, newFakeStore())
	if _, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302"}}, time.Hour, "7000112", "test"); err != nil {
		t.Fatalf("applyJail: %v", err)
	}

	if !slices.Contains(ops.voiceKickCalls, "7000102") {
		t.Errorf("applyJail did not disconnect the newly jailed member from voice; calls = %v", ops.voiceKickCalls)
	}
}

// reapplyIfEvaded is the rejoin-evasion path.
func TestReapplyIfEvadedDisconnectsFromVoice(t *testing.T) {
	ops := newFakeOps()
	ops.setMemberJoined("7000001", "7000102", nil, fixedNow.Add(-time.Minute))

	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("7000102", []string{"7000302"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	if err := p.reapplyEvadedJails(context.Background(), "7000001"); err != nil {
		t.Fatalf("reapplyEvadedJails: %v", err)
	}

	if !slices.Contains(ops.voiceKickCalls, "7000102") {
		t.Errorf("reapplyIfEvaded did not disconnect the re-jailed rejoiner from voice; calls = %v", ops.voiceKickCalls)
	}
}

// HandleMemberUpdate is the onboarding/screening regrant path.
func TestHandleMemberUpdateDisconnectsFromVoice(t *testing.T) {
	ops := newFakeOps()
	ops.setMemberJoined("7000001", "7000102", []string{"7000301", "7000318"}, jailedAt.Add(-24*time.Hour))

	store := newFakeStore()
	if err := store.InsertJail(context.Background(), activeJail("7000102", []string{"7000302"})); err != nil {
		t.Fatalf("InsertJail: %v", err)
	}

	p := newEvasionPlugin(t, ops, store)
	p.HandleMemberUpdate(context.Background(), "7000001", "7000102", []string{"7000301", "7000318"})

	if !slices.Contains(ops.voiceKickCalls, "7000102") {
		t.Errorf("HandleMemberUpdate did not disconnect the reasserted jail from voice; calls = %v", ops.voiceKickCalls)
	}
}

// A voice-disconnect is best-effort: Discord requires Move Members for it
// (a separate permission from the Manage Roles the strip itself needs), and
// a guild that hasn't granted it must not lose jail's core role-strip over a
// permission jail never needed before. The failure is swallowed, not
// propagated.
func TestVoiceDisconnectFailureDoesNotFailJail(t *testing.T) {
	ops := newFakeOps()
	ops.setMember("7000001", "7000102", []string{"7000302"})
	ops.voiceKickErr = errors.New("missing Move Members permission")

	p := newEvasionPlugin(t, ops, newFakeStore())
	unmanageable, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102", roles: []string{"7000302"}}, time.Hour, "7000112", "test")
	if err != nil {
		t.Fatalf("applyJail must succeed despite a failing voice kick: %v", err)
	}
	if len(unmanageable) != 0 {
		t.Fatalf("unexpected unmanageable roles: %v", unmanageable)
	}

	member, gerr := ops.GuildMember("7000001", "7000102")
	if gerr != nil {
		t.Fatalf("GuildMember: %v", gerr)
	}
	if !slices.Contains(member.Roles, "7000301") {
		t.Errorf("role strip did not apply despite the voice kick failing separately; roles = %v", member.Roles)
	}
}
