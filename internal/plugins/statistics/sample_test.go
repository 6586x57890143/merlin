package statistics

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRenderSamples writes realistic renders to ACTIVITY_SAMPLE_DIR for
// eyeballing a layout change, and checks nothing: the geometry tests do
// that, and this is the half a person has to look at. Skipped otherwise.
func TestRenderSamples(t *testing.T) {
	dir := os.Getenv("ACTIVITY_SAMPLE_DIR")
	if dir == "" {
		t.Skip("set ACTIVITY_SAMPLE_DIR to write sample renders")
	}
	client := stubCDN(t)
	names := []string{"beloved🐻‍❄️freak", "Yung Eepy", "funjord", "jeez", "emmi", "no im not", "NotWolf", "FMJAY716-KICK.COM",
		"Enosh", "alexiane", "spooky zoe 👻🔮🎃", "Celaena mother of dragons and cats", "nu-male", "Jojo", "RICK🤬LAS RAGE (ALT)",
		"Reshi", "COLORFULFLOWER", "Honey (male slayer)", "Mollystlk", "discord.gg/melted", "Kaiser", "jazmin", "NeoSyk",
		"duckbringer", "nurock", "BabyPeach", "GiftLuck", "Laohu (Currently 1-0)", "glow", "Bank", "MoarMatt", "sewafina",
		"Foxy", "DALIBAN COMMANDER", "Scratchy", "Shwueen"}
	channels := []string{"✨-general-chat", "✨-general-chat-archive-2026-09-14", "ticket-0017", "☕-break-room", "gen-1", "Pot vs. NeoSyk"}
	var people []*person
	for i, n := range names {
		p := &person{id: fmt.Sprintf("8035111022467%04d", i), name: n, avatar: "x", count: 480 - i*12 - (i*7)%9, channels: map[string]bool{}}
		for j := 0; j <= i%3; j++ {
			p.channels[channels[(i+j)%len(channels)]] = true
		}
		if i%6 == 3 || i == 0 {
			p.voice = time.Duration(30+i*11) * time.Minute
			p.rooms = map[string]bool{"🎧 lounge": true}
		}
		people = append(people, p)
	}
	people = append(people,
		&person{id: "80351110224679001", name: "mic only", voice: 4 * time.Hour, channels: map[string]bool{}, rooms: map[string]bool{"🎧 lounge": true, "gaming": true}},
		&person{id: "80351110224679002", name: "quiet listener", voice: 95 * time.Minute, channels: map[string]bool{}, rooms: map[string]bool{"afk": true}},
	)
	byID := map[string]*person{}
	for _, p := range people {
		byID[p.id] = p
	}

	day := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	var hours []DayStat
	for h := range 12 {
		hours = append(hours, DayStat{Day: day.Add(time.Duration(h) * time.Hour), Messages: (h*h*7 + 3) % 900, VoiceSeconds: ((h * 13) % 5) * 1800})
	}
	today := report{people: rank(byID), messages: 7476, voice: 9*time.Hour + 20*time.Minute, channels: 42, hourly: true, days: hours}
	write := func(name string, rep report, start, end time.Time, top int) {
		body, err := renderPNG(client, rep, "The Melting Pot", start, end, top)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("sample-today.png", today, day, day.Add(11*time.Hour+29*time.Minute), 24)

	var week []DayStat
	for h := range 7 * 24 {
		week = append(week, DayStat{Day: day.AddDate(0, 0, -6).Add(time.Duration(h) * time.Hour), Messages: (h*h*7 + 3) % 500, VoiceSeconds: ((h * 13) % 5) * 1800})
	}
	wk := today
	wk.days = week
	write("sample-week.png", wk, day.AddDate(0, 0, -6), day.Add(12*time.Hour), 24)

	var days []DayStat
	for n := range 90 {
		days = append(days, DayStat{Day: day.AddDate(0, 0, n-89), Messages: (n*37)%900 + 100, VoiceSeconds: ((n * 53) % 7) * 3600})
	}
	quarter := today
	quarter.hourly, quarter.days = false, days
	write("sample-quarter.png", quarter, day.AddDate(0, 0, -89), day.Add(12*time.Hour), 24)

	write("sample-one.png", report{people: people[:1], messages: 480, voice: 30 * time.Minute, channels: 1, hourly: true, days: hours}, day, day.Add(11*time.Hour), 24)
}
