package statistics

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	snow "github.com/disgoorg/snowflake/v2"
)

type fakeGuilds struct{ members int }

func (f fakeGuilds) GetGuild(snow.ID, bool, ...rest.RequestOpt) (*discord.RestGuild, error) {
	g := &discord.RestGuild{}
	g.Name = "the pot"
	g.ApproximateMemberCount = f.members
	return g, nil
}

// TestMemberPointsWalkBackFromToday: the population at the end of a day is
// today's count less everything that happened after it, including days
// after the window, and nothing is claimed for a day before counting began.
func TestMemberPointsWalkBackFromToday(t *testing.T) {
	d := func(n int) time.Time { return dayOne.AddDate(0, 0, n) }
	since := []MemberDay{
		{Day: d(1), Joined: 5, Departed: 1}, // +4
		{Day: d(2), Joined: 0, Departed: 2}, // -2
		{Day: d(4), Joined: 10},             // after the window: +10
	}
	pts := memberPoints(since[:2], since, d(0), d(3), d(1), 100)
	if len(pts) != 3 {
		t.Fatalf("three days, got %d", len(pts))
	}
	if !math.IsNaN(pts[0].pop) || pts[0].state != dayNotCounted {
		t.Fatalf("day 0 is before counting: %+v", pts[0])
	}
	// 100 now; 90 before the +10 after the window; 92 before day 2's -2.
	if pts[1].pop != 92 || pts[2].pop != 90 {
		t.Fatalf("got %v, %v; want 92, 90", pts[1].pop, pts[2].pop)
	}
	if pts[1].joined != 5 || pts[2].departed != 2 {
		t.Fatalf("bars come from the window: %+v %+v", pts[1], pts[2])
	}
	if pts := memberPoints(since[:2], since, d(0), d(3), d(1), 0); !math.IsNaN(pts[1].pop) {
		t.Fatal("no count from Discord, no population")
	}
}

func TestHandleMembersDrawsThePopulation(t *testing.T) {
	p, store := seeded(t)
	p.guilds = fakeGuilds{members: 250}
	for i := 1; i <= 20; i++ {
		_ = store.AddMembers(context.Background(), []MemberBucket{{GuildID: guild, Hour: dayOne.AddDate(0, 0, i).Add(10 * time.Hour), Joined: i % 7, Departed: i % 3}})
	}
	p.now = func() time.Time { return dayOne.AddDate(0, 0, 21).Add(6 * time.Hour) }
	ix, w := run(t, admin, "members", strArg("from", "2026-08-25"))
	p.handleMembers(context.Background(), ix)
	if !said(w, `filename="members.png"`) || !said(w, `filename="members.csv"`) {
		t.Fatalf("chart and csv attach: %v", w.Bodies)
	}
	if !said(w, "`250` now") {
		t.Fatalf("names the count it worked back from: %v", w.Bodies)
	}

	window, _ := store.MemberDays(context.Background(), guild, dayOne.AddDate(0, 0, -6), p.now())
	pts := memberPoints(window, window, dayOne.AddDate(0, 0, -6), p.now(), windowStart, 250)
	png, err := renderMembers(nil, pts, "the pot", dayOne.AddDate(0, 0, -6), p.now(), 250)
	if err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("ACTIVITY_SAMPLE_DIR"); dir != "" {
		_ = os.WriteFile(filepath.Join(dir, "members.png"), png, 0o644)
	}
}
