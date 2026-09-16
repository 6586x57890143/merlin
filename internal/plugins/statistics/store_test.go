package statistics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/dbtest"
)

// The fake in fakes_test.go carries the counter, the backfill and the
// report; what it cannot check is the SQL: the unnest bulk upserts, the
// additive versus replacing writes, the newest-sighting-wins predicate, the
// per-guild retention join in Prune, and the aggregate shapes the report
// queries scan into. Skips without TEST_DATABASE_URL; CI runs it against
// postgres:16-alpine.

func pgTestStore(t *testing.T) (Store, string) {
	t.Helper()
	pool := dbtest.Pool(t)
	var b [6]byte
	_, _ = rand.Read(b[:])
	return NewPostgresStore(pool), "g-" + hex.EncodeToString(b[:])
}

func TestPostgresStoreRoundTrip(t *testing.T) {
	s, g := pgTestStore(t)
	ctx := context.Background()
	h0 := time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)
	h1 := h0.Add(time.Hour)

	// Live writes add.
	if err := s.AddMessages(ctx, []Bucket{
		{GuildID: g, ChannelID: "c1", UserID: "u1", Hour: h0, Messages: 2},
		{GuildID: g, ChannelID: "c1", UserID: "u2", Hour: h0, Messages: 1},
		{GuildID: g, ChannelID: "c2", UserID: "u1", Hour: h1, Messages: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessages(ctx, []Bucket{{GuildID: g, ChannelID: "c1", UserID: "u1", Hour: h0, Messages: 5}}); err != nil {
		t.Fatal(err)
	}
	// Backfill writes replace the whole channel-hour.
	if err := s.SetHour(ctx, g, "c1", h0, map[string]int{"u1": 4, "u3": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHour(ctx, g, "c3", h1, nil); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Report(ctx, g, "", h0, h1.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Row{}
	for _, r := range rows {
		got[r.UserID] = r
	}
	if got["u1"].Messages != 7 || len(got["u1"].Channels) != 2 || !got["u1"].Last.Equal(h1) {
		t.Fatalf("u1: %+v", got["u1"])
	}
	if _, ok := got["u2"]; ok {
		t.Fatal("SetHour should have replaced u2's row in that hour")
	}
	if got["u3"].Messages != 1 {
		t.Fatalf("u3: %+v", got["u3"])
	}
	only, err := s.Report(ctx, g, "c2", h0, h1.Add(time.Hour))
	if err != nil || len(only) != 1 || only[0].Messages != 3 {
		t.Fatalf("channel filter: %+v %v", only, err)
	}

	totals, err := s.ChannelTotals(ctx, g, h0, h1.Add(time.Hour))
	if err != nil || len(totals) != 2 || totals[0].ChannelID != "c1" || totals[0].Messages != 5 || totals[0].People != 2 {
		t.Fatalf("channel totals: %+v %v", totals, err)
	}
	oldest, err := s.OldestHour(ctx, g)
	if err != nil || !oldest.Equal(h0) {
		t.Fatalf("oldest: %v %v", oldest, err)
	}
	if none, err := s.OldestHour(ctx, g+"-empty"); err != nil || !none.IsZero() {
		t.Fatalf("no rows should read as zero: %v %v", none, err)
	}

	// Newest sighting wins, regardless of write order.
	if err := s.UpsertUsers(ctx, []UserSeen{
		{GuildID: g, UserID: "u1", Name: "new", SeenAt: h1},
		{GuildID: g, UserID: "u1", Name: "old", SeenAt: h0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertUsers(ctx, []UserSeen{{GuildID: g, UserID: "u1", Name: "older", SeenAt: h0}}); err != nil {
		t.Fatal(err)
	}
	users, err := s.Users(ctx, g, []string{"u1", "nobody"})
	if err != nil || users["u1"].Name != "new" || len(users) != 1 {
		t.Fatalf("users: %+v %v", users, err)
	}
	if err := s.UpsertChannels(ctx, []ChannelSeen{{GuildID: g, ChannelID: "c1", Name: "general"}, {GuildID: g, ChannelID: "c1", Name: ""}}); err != nil {
		t.Fatal(err)
	}
	names, err := s.Channels(ctx, g)
	if err != nil || names["c1"] != "general" {
		t.Fatalf("channels: %+v %v", names, err)
	}

	// Members add per hour and roll up per day.
	if err := s.AddMembers(ctx, []MemberBucket{{GuildID: g, Hour: h0, Joined: 2}, {GuildID: g, Hour: h1, Departed: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMembers(ctx, []MemberBucket{{GuildID: g, Hour: h0, Joined: 1}}); err != nil {
		t.Fatal(err)
	}
	days, err := s.MemberDays(ctx, g, h0, h1.Add(time.Hour))
	if err != nil || len(days) != 1 || days[0].Joined != 3 || days[0].Departed != 1 {
		t.Fatalf("member days: %+v %v", days, err)
	}

	// Voice adds like messages, joins onto the report for members with or
	// without messages, and rolls up per day beside them.
	if err := s.AddVoice(ctx, []VoiceBucket{
		{GuildID: g, ChannelID: "v1", UserID: "u1", Hour: h0, Seconds: 600},
		{GuildID: g, ChannelID: "v1", UserID: "u9", Hour: h1, Seconds: 3600},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddVoice(ctx, []VoiceBucket{{GuildID: g, ChannelID: "v1", UserID: "u1", Hour: h0, Seconds: 300}}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Report(ctx, g, "", h0, h1.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]Row{}
	for _, r := range rows {
		got[r.UserID] = r
	}
	if got["u1"].Messages != 7 || got["u1"].VoiceSeconds != 900 {
		t.Fatalf("u1 with voice: %+v", got["u1"])
	}
	if r := got["u9"]; r.Messages != 0 || r.VoiceSeconds != 3600 || len(r.Channels) != 0 || !r.Last.IsZero() {
		t.Fatalf("voice-only member: %+v", r)
	}
	stats, err := s.Days(ctx, g, "", h0, h1.Add(time.Hour))
	if err != nil || len(stats) != 1 || stats[0].Messages != 8 || stats[0].VoiceSeconds != 4500 || !stats[0].Day.Equal(h0.Truncate(24*time.Hour)) {
		t.Fatalf("days: %+v %v", stats, err)
	}
	if stats, err = s.Days(ctx, g, "v1", h0, h1.Add(time.Hour)); err != nil || len(stats) != 1 || stats[0].Messages != 0 || stats[0].VoiceSeconds != 4500 {
		t.Fatalf("days for a voice channel: %+v %v", stats, err)
	}
}

func TestPostgresStoreConfigAndPrune(t *testing.T) {
	s, g := pgTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	cfg, err := s.Config(ctx, g)
	if err != nil || cfg.RetentionDays != defaultRetentionDays || !cfg.LiveSince.IsZero() {
		t.Fatalf("default config: %+v %v", cfg, err)
	}
	if err := s.MarkLive(ctx, g, now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkLive(ctx, g, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRetention(ctx, g, 7); err != nil {
		t.Fatal(err)
	}
	cfg, err = s.Config(ctx, g)
	if err != nil || cfg.RetentionDays != 7 || !cfg.LiveSince.Equal(now) {
		t.Fatalf("config after set: %+v %v (live_since must not move)", cfg, err)
	}

	other := g + "-default"
	if err := s.AddMessages(ctx, []Bucket{
		{GuildID: g, ChannelID: "c", UserID: "u", Hour: now.Add(-10 * 24 * time.Hour), Messages: 1},
		{GuildID: g, ChannelID: "c", UserID: "u", Hour: now.Add(-time.Hour), Messages: 1},
		{GuildID: other, ChannelID: "c", UserID: "u", Hour: now.Add(-10 * 24 * time.Hour), Messages: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMembers(ctx, []MemberBucket{{GuildID: g, Hour: now.Add(-10 * 24 * time.Hour), Joined: 1}}); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("expected the 7-day guild's old message row and member row pruned, got %d", n)
	}
	rows, _ := s.Report(ctx, g, "", now.Add(-30*24*time.Hour), now)
	if len(rows) != 1 || rows[0].Messages != 1 {
		t.Fatalf("7-day guild should keep only the recent row: %+v", rows)
	}
	rows, _ = s.Report(ctx, other, "", now.Add(-30*24*time.Hour), now)
	if len(rows) != 1 {
		t.Fatalf("a guild on the 90-day default must keep a 10-day-old row: %+v", rows)
	}
}

func TestPostgresStoreBackfillQueue(t *testing.T) {
	s, g := pgTestStore(t)
	ctx := context.Background()
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	until := from.Add(24 * time.Hour)

	if err := s.RequestBackfill(ctx, []BackfillRow{
		{GuildID: g, ChannelID: "a", From: from, Until: until},
		{GuildID: g, ChannelID: "b", From: from, Until: until},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingBackfill(ctx, g)
	if err != nil || len(pending) != 2 || pending[0].ChannelID != "a" || !pending[0].From.Equal(from) {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	if err := s.SetBackfillCursor(ctx, g, "a", "12345", false, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBackfillCursor(ctx, g, "b", "", true, "Missing Access"); err != nil {
		t.Fatal(err)
	}
	pending, _ = s.PendingBackfill(ctx, g)
	if len(pending) != 1 || pending[0].Cursor != "12345" {
		t.Fatalf("cursor: %+v", pending)
	}
	sum, err := s.BackfillStatus(ctx, g)
	if err != nil || sum.Pending != 1 || sum.Failed != 1 || sum.Done != 0 || !sum.From.Equal(from) {
		t.Fatalf("status: %+v %v", sum, err)
	}
	// A second request restarts a channel from scratch.
	if err := s.RequestBackfill(ctx, []BackfillRow{{GuildID: g, ChannelID: "b", From: from, Until: until}}); err != nil {
		t.Fatal(err)
	}
	sum, _ = s.BackfillStatus(ctx, g)
	if sum.Pending != 2 || sum.Failed != 0 {
		t.Fatalf("re-request should reset the row: %+v", sum)
	}
	if none, err := s.BackfillStatus(ctx, g+"-none"); err != nil || none.Pending != 0 || !none.From.IsZero() {
		t.Fatalf("empty status: %+v %v", none, err)
	}
}
