package roles

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/core/ui"
	"github.com/disgoorg/disgo/discord"
)

func TestUnconfirmedJailRetainsRestorationSnapshot(t *testing.T) {
	for _, duration := range []time.Duration{0, 5 * time.Minute} {
		t.Run(duration.String(), func(t *testing.T) {
			ops := newFakeOps()
			ops.setMember("7000001", "7000102", []string{"7000302"})
			ops.roles["7000001"] = []*frole{{ID: "7000302"}, {ID: "7000301"}}
			ops.memberEditResponseErr = errors.New("response lost after Discord applied edit")
			p, timers, now := newTimedPlugin(ops, newFakeStore())
			_, err := p.applyJail(context.Background(), "7000001", "7000301", jailTarget{userID: "7000102"}, duration, "7000319", "")
			if err == nil {
				t.Fatal("expected unconfirmed edit error")
			}
			rec, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102")
			if !ok || len(rec.SnapshotRoleIDs) != 1 || rec.SnapshotRoleIDs[0] != "7000302" {
				t.Fatalf("lost restoration snapshot: %+v", rec)
			}
			member, _ := ops.GuildMember("7000001", "7000102")
			if len(member.Roles) != 1 || member.Roles[0] != "7000301" {
				t.Fatalf("edit was not applied: %v", member.Roles)
			}
			ops.memberEditResponseErr = nil
			if duration > 0 {
				if len(timers.armed) != 1 || timers.armed[0].delay != duration {
					t.Fatalf("release not armed: %+v", timers.armed)
				}
				*now = fixedNow.Add(duration)
				timers.armed[0].fire()
			} else if err := p.releaseJail(context.Background(), "7000001", "7000102", rec, "7000319"); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := p.store.GetJail(context.Background(), "7000001", "7000102"); ok {
				t.Fatal("release retained record")
			}
			member, _ = ops.GuildMember("7000001", "7000102")
			if len(member.Roles) != 1 || member.Roles[0] != "7000302" {
				t.Fatalf("roles not restored: %v", member.Roles)
			}
		})
	}
}

func TestMaximumAllowlistReplacementFitsSettingsTextBudget(t *testing.T) {
	for _, page := range []string{"jail", "vacation"} {
		for _, syncFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failure=%t", page, syncFails), func(t *testing.T) {
				p, ops, _, _, _, settings := handlerFixture()
				var readOnly, previous, picked []string
				for i := range 25 {
					readOnly = append(readOnly, fmt.Sprintf("1000000000000000%03d", i))
					previous = append(previous, fmt.Sprintf("1000000000000001%03d", i))
					id := fmt.Sprintf("1000000000000002%03d", i)
					picked = append(picked, id)
					ops.channel[id] = &fch{ID: id, GuildID: "7000001", Type: discord.ChannelTypeGuildText}
				}
				customID := panelJail + "conversation"
				add, remove := settings.AddJailConversationChannel, settings.RemoveJailConversationChannel
				settings.allowed["7000001"], settings.conversation["7000001"] = readOnly, previous
				if page == "vacation" {
					customID = panelJail + "vacconversation"
					add, remove = settings.AddVacationConversationChannel, settings.RemoveVacationConversationChannel
					settings.vacationAllowed["7000001"], settings.vacationConversation["7000001"] = readOnly, previous
				}
				var options []discord.StringSelectMenuOption
				for _, id := range previous {
					options = append(options, discord.StringSelectMenuOption{Label: id, Value: id, Default: true})
				}
				drawn := core.Panel{Parts: []discord.ContainerSubComponent{ui.Row(discord.StringSelectMenuComponent{CustomID: customID, Options: options})}}
				ix := decodeIx(3, "7000001", "7000101", map[string]any{"custom_id": customID, "component_type": 3, "values": picked}, map[string]any{"id": "102", "channel_id": "101", "author": map[string]any{"id": "2", "username": "merlin"}, "components": drawn.Layout()})
				note := p.setAllowlist(context.Background(), ix, customID, "members", add, remove, "test", func(string, string) (discord.Permissions, error) {
					if syncFails {
						return 0, errors.New(strings.Repeat("permission denied ", 100))
					}
					return 0, nil
				})
				if !strings.Contains(note, "25 additions and 25 removals") {
					t.Fatalf("missing complete save summary: %s", note)
				}
				if syncFails && !strings.Contains(note, "not synced") {
					t.Fatal("sync failure hidden")
				}
				text := paneltest.IxTexts(p.panel(context.Background(), ix, page, "", note))
				if size := len([]rune(text)); size > 4000 {
					t.Fatalf("rendered settings has %d characters, exceeding 4000", size)
				}
			})
		}
	}
}
