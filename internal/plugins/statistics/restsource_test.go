package statistics

import (
	"encoding/json"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	snow "github.com/disgoorg/snowflake/v2"
)

// fakeHistory answers restSource from Discord's JSON.
type fakeHistory struct {
	channels, threads, messages string
	before                      snow.ID
}

func (h *fakeHistory) GetGuildChannels(snow.ID, ...rest.RequestOpt) ([]discord.GuildChannel, error) {
	var raw []discord.UnmarshalChannel
	if err := json.Unmarshal([]byte(h.channels), &raw); err != nil {
		return nil, err
	}
	out := make([]discord.GuildChannel, len(raw))
	for i, c := range raw {
		out[i] = c.Channel.(discord.GuildChannel)
	}
	return out, nil
}

func (h *fakeHistory) GetActiveGuildThreads(snow.ID, ...rest.RequestOpt) (*discord.GuildActiveThreads, error) {
	var t discord.GuildActiveThreads
	return &t, json.Unmarshal([]byte(h.threads), &t)
}

func (h *fakeHistory) GetMessages(_, _, before, _ snow.ID, _ int, _ ...rest.RequestOpt) ([]discord.Message, error) {
	h.before = before
	var m []discord.Message
	return m, json.Unmarshal([]byte(h.messages), &m)
}

// The backfill lists a server's text channels and its active threads, from
// the guild's own endpoint, and reads a page of messages as the live
// counter reads one.
func TestRestSourceReadsChannelsThreadsAndMessages(t *testing.T) {
	h := &fakeHistory{
		channels: `[{"id":"10","type":0,"guild_id":"3","name":"general"},{"id":"11","type":2,"guild_id":"3","name":"voice"},
			{"id":"12","type":4,"guild_id":"3","name":"cat"}]`,
		threads: `{"threads":[{"id":"20","type":11,"guild_id":"3","parent_id":"10","name":"a thread",
			"thread_metadata":{"archived":false,"auto_archive_duration":60,"archive_timestamp":"2026-01-01T00:00:00Z","locked":false}}],"members":[]}`,
		messages: `[{"id":"31","channel_id":"10","author":{"id":"4","username":"u","global_name":"Zoe","avatar":"abc"},"timestamp":"2026-01-01T00:00:00Z"},
			{"id":"30","channel_id":"10","author":{"id":"5","username":"hook"},"webhook_id":"9","timestamp":"2026-01-01T00:00:00Z"}]`,
	}
	s := restSource{h}
	chans, err := s.channelList("3")
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 2 || chans[0].ChannelID != "10" || chans[1].ChannelID != "20" || chans[1].Name != "a thread" {
		t.Fatalf("channels %+v", chans)
	}
	page, err := s.messagePage("10", "99", 100)
	if err != nil {
		t.Fatal(err)
	}
	if h.before != 99 || len(page) != 2 || page[0].id != "31" || page[0].authorID != "4" || page[0].name != "Zoe" ||
		page[0].avatar != "abc" || page[0].skip || !page[1].skip {
		t.Fatalf("before %d, page %+v", h.before, page)
	}
}
