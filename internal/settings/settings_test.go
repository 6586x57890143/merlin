package settings

import "testing"

func TestIsConfiguredFalseForEmptyGuild(t *testing.T) {
	gs := GuildSettings{GuildID: "g1"}
	if gs.IsConfigured() {
		t.Fatal("expected a guild with nothing set to be unconfigured")
	}
}

func TestIsConfiguredTrueForEachField(t *testing.T) {
	cases := []struct {
		name string
		gs   GuildSettings
	}{
		{"audit channel", GuildSettings{AuditLogChannelID: "c1"}},
		{"status channel", GuildSettings{StatusChannelID: "c1"}},
		{"mod role", GuildSettings{ModRoleIDs: []string{"r1"}}},
		{"admin", GuildSettings{AdminUserIDs: []string{"u1"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.gs.IsConfigured() {
				t.Fatalf("expected IsConfigured to be true when only %s is set", c.name)
			}
		})
	}
}

func TestDisclosureValid(t *testing.T) {
	for _, d := range []Disclosure{DisclosureFull, DisclosureCadence, DisclosureRetention, DisclosureGeneric} {
		if !d.Valid() {
			t.Errorf("Disclosure(%q).Valid() = false, want true", d)
		}
	}
	// The empty value is deliberately not valid on its own: a caller that
	// means "unspecified" is expected to call Resolve first, so that a mode
	// arriving straight from a corrupt row is caught rather than silently
	// read as the most disclosing option.
	for _, d := range []Disclosure{"", "everything", "FULL"} {
		if d.Valid() {
			t.Errorf("Disclosure(%q).Valid() = true, want false", d)
		}
	}
}

func TestDisclosureResolve(t *testing.T) {
	if got := Disclosure("").Resolve(); got != DisclosureFull {
		t.Errorf(`Disclosure("").Resolve() = %q, want full`, got)
	}
	for _, d := range []Disclosure{DisclosureFull, DisclosureCadence, DisclosureRetention, DisclosureGeneric} {
		if got := d.Resolve(); got != d {
			t.Errorf("Disclosure(%q).Resolve() = %q, want unchanged", d, got)
		}
	}
	// An unrecognized non-empty value passes through unchanged, leaving
	// Valid to reject it, rather than Resolve silently laundering a typo
	// into a legal mode.
	if got := Disclosure("everything").Resolve(); got != "everything" {
		t.Errorf(`Disclosure("everything").Resolve() = %q, want unchanged`, got)
	}
}
