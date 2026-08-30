package rotation

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/merlin/internal/settings"
)

// TestValidateRotationChannelAcceptsNilRetention is a regression test for
// the reported bug where retention wasn't really optional: a rotation
// config with RetentionHours left nil (the "keep forever" case /rotation
// configure add produces when the retention option is simply omitted) must
// pass validation cleanly, not be treated as if something required was
// missing.
func TestValidateRotationChannelAcceptsNilRetention(t *testing.T) {
	rc := settings.RotationChannel{
		GuildID: "g1", ChannelID: "c1", IntervalMinutes: 24 * 60,
		ArchiveCategoryID: "cat1", ArchiveVisibility: "mod_only",
		RetentionHours: nil,
	}
	if err := ValidateChannel(rc); err != nil {
		t.Fatalf("expected a nil (forever) retention to be valid, got %v", err)
	}
}

func TestValidateRotationChannelRejectsZeroRetention(t *testing.T) {
	rc := settings.RotationChannel{
		GuildID: "g1", ChannelID: "c1", IntervalMinutes: 24 * 60,
		ArchiveCategoryID: "cat1", ArchiveVisibility: "mod_only",
		RetentionHours: intPtr(0),
	}
	if err := ValidateChannel(rc); err == nil {
		t.Fatal("expected an explicit 0-hour retention to be rejected (use nil/omit for forever instead)")
	}
}

func fakeChannelOpt(name, channelID string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionChannel, Value: channelID}
}

// TestResolveArchiveCategoryUsesGivenOption is a regression test for the
// second half of the same reported bug: archive_category used to be
// required, forcing a mod to go create a category in Discord's UI before
// they could ever run /rotation configure add at all. It's optional now,
// this covers the case where one IS supplied, which must still just use it
// as-is rather than ignoring it in favor of the auto-create fallback.
func TestResolveArchiveCategoryUsesGivenOption(t *testing.T) {
	ops, _, _, p, _ := setupRotation(t, finiteRetentionRC())
	opts := map[string]*discordgo.ApplicationCommandInteractionDataOption{
		"archive_category": fakeChannelOpt("archive_category", "explicit-cat"),
	}

	got, err := p.resolveArchiveCategory("g1", opts)
	if err != nil {
		t.Fatalf("resolveArchiveCategory: %v", err)
	}
	if got != "explicit-cat" {
		t.Fatalf("expected the explicitly-given category ID, got %q", got)
	}
	if _, err := ops.Channel("explicit-cat"); err == nil {
		t.Fatal("expected no new category to have been created when one was already given")
	}
}

func TestResolveArchiveCategoryReusesExistingArchiveCategory(t *testing.T) {
	ops, _, _, p, _ := setupRotation(t, finiteRetentionRC())
	ops.addChannel(&discordgo.Channel{ID: "existing-archive-cat", GuildID: "g1", Name: defaultArchiveCategoryName, Type: discordgo.ChannelTypeGuildCategory})

	got, err := p.resolveArchiveCategory("g1", map[string]*discordgo.ApplicationCommandInteractionDataOption{})
	if err != nil {
		t.Fatalf("resolveArchiveCategory: %v", err)
	}
	if got != "existing-archive-cat" {
		t.Fatalf("expected the existing %q category to be reused, got %q", defaultArchiveCategoryName, got)
	}
}

func TestResolveArchiveCategoryCreatesArchiveCategoryWhenMissing(t *testing.T) {
	ops, _, _, p, _ := setupRotation(t, finiteRetentionRC())

	got, err := p.resolveArchiveCategory("g1", map[string]*discordgo.ApplicationCommandInteractionDataOption{})
	if err != nil {
		t.Fatalf("resolveArchiveCategory: %v", err)
	}
	created, err := ops.Channel(got)
	if err != nil {
		t.Fatalf("expected a new category to have been created, got err: %v", err)
	}
	if created.Name != defaultArchiveCategoryName || created.Type != discordgo.ChannelTypeGuildCategory {
		t.Fatalf("expected a category named %q, got name=%q type=%v", defaultArchiveCategoryName, created.Name, created.Type)
	}
}

// Disclosure decides what a channel publishes about itself, so a typo has to
// fail at the point of mutation rather than being stored and silently read as
// some other mode.
func TestValidateRejectsAnUnknownDisclosure(t *testing.T) {
	base := settings.RotationChannel{
		ChannelID: "c1", ArchiveCategoryID: "cat1", ArchiveVisibility: "mod_only",
		IntervalMinutes: 24 * 60, NoticeLeadMinutes: 10,
	}

	for _, d := range []settings.Disclosure{
		settings.DisclosureFull, settings.DisclosureCadence,
		settings.DisclosureRetention, settings.DisclosureGeneric,
		// Unset is today's behaviour, so it has to keep validating: every
		// in-memory RotationChannel literal leaves it empty.
		"",
	} {
		rc := base
		rc.Disclosure = d
		if err := ValidateChannel(rc); err != nil {
			t.Errorf("disclosure %q was rejected: %v", d, err)
		}
	}

	rc := base
	rc.Disclosure = "everything"
	if err := ValidateChannel(rc); err == nil {
		t.Error("an unknown disclosure mode was accepted")
	}
}

// The whitelist visibility is no longer offered by /rotation configure, but
// guilds carry it from /config import and archiveOverwrites still implements
// it. Rejecting it here would lock those guilds out of editing the channel at
// all, since edit is a read-modify-write of the whole struct: an admin
// changing only the interval would be refused over a field they never touched
// and can no longer even select.
func TestLegacyWhitelistVisibilityStillValidates(t *testing.T) {
	rc := settings.RotationChannel{
		ChannelID: "c1", ArchiveCategoryID: "cat1", ArchiveVisibility: "whitelist",
		IntervalMinutes: 24 * 60, NoticeLeadMinutes: 10,
	}
	if err := ValidateChannel(rc); err != nil {
		t.Errorf("a legacy imported whitelist slot can no longer be edited: %v", err)
	}
}

// The audit trail is where somebody looks after a channel published something
// unexpected, so it has to record which mode was in force.
func TestRotationSummaryRecordsTheDisclosure(t *testing.T) {
	got := rotationSummary(settings.RotationChannel{
		IntervalMinutes: 24 * 60, ArchiveVisibility: "mod_only", Disclosure: settings.DisclosureCadence,
	})
	if !strings.Contains(got, "disclosure=cadence") {
		t.Errorf("audit summary does not record the disclosure mode: %s", got)
	}
	// Unset has to render as what it actually means, not as an empty value.
	got = rotationSummary(settings.RotationChannel{IntervalMinutes: 60, ArchiveVisibility: "mod_only"})
	if !strings.Contains(got, "disclosure=full") {
		t.Errorf("an unset disclosure did not render as its effective value: %s", got)
	}
}
