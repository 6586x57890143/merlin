package fleetgrant

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"

	"github.com/6586x57890143/cygnet/link"

	"github.com/6586x57890143/merlin/internal/core"
	"github.com/6586x57890143/merlin/internal/core/paneltest"
	"github.com/6586x57890143/merlin/internal/discordguard"
)

const (
	guild = "100000000000000001"
	user  = "100000000000000002"
	role  = "100000000000000003"
	other = "100000000000000004"
)

type memStore struct{ rows map[[3]string]string }

func (s *memStore) Allow(_ context.Context, g, c, r, by string) error {
	s.rows[[3]string{g, c, r}] = by
	return nil
}

func (s *memStore) Deny(_ context.Context, g, c, r string) (bool, error) {
	_, had := s.rows[[3]string{g, c, r}]
	delete(s.rows, [3]string{g, c, r})
	return had, nil
}

func (s *memStore) Allowed(_ context.Context, g, c, r string) (bool, error) {
	_, ok := s.rows[[3]string{g, c, r}]
	return ok, nil
}

func (s *memStore) List(_ context.Context, g string) ([]Entry, error) {
	var out []Entry
	for k, by := range s.rows {
		if k[0] == g {
			out = append(out, Entry{Caller: k[1], RoleID: k[2], AddedBy: by})
		}
	}
	return out, nil
}

type fakeRoles struct{ bare error }

func (f fakeRoles) BareRole(string, string) error { return f.bare }

type fakeUse map[string]bool

func (f fakeUse) RoleInUse(_, r string) bool { return f[r] }

type fakeOps struct {
	member  *discord.Member
	err     error
	added   []snowflake.ID
	reasons int
}

func (f *fakeOps) GetMember(snowflake.ID, snowflake.ID) (*discord.Member, error) {
	if f.member == nil {
		return nil, errors.New("unknown member")
	}
	return f.member, nil
}

func (f *fakeOps) AddMemberRole(_, _, r snowflake.ID, opts ...rest.RequestOpt) error {
	if f.err != nil {
		return f.err
	}
	f.added = append(f.added, r)
	f.reasons += len(opts)
	return nil
}

type audits struct{ actions []string }

func (a *audits) Record(_ context.Context, _, actor, action, _, _ string) error {
	a.actions = append(a.actions, actor+" "+action)
	return nil
}

func plugin() (*Plugin, *memStore, *fakeOps, *audits, fakeUse) {
	st, ops, au, use := &memStore{rows: map[[3]string]string{}}, &fakeOps{member: &discord.Member{User: discord.User{ID: snowflake.MustParse(user)}}}, &audits{}, fakeUse{}
	p := New(st, use, func(string) MemberOps { return ops })
	p.roles, p.audit, p.log = fakeRoles{}, au, slog.New(slog.NewTextHandler(io.Discard, nil))
	p.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return p, st, ops, au, use
}

// A bird gets a role only when it was allowed exactly that role, the role
// is bare and unused by merlin's settings, and the member is a person; it
// is added once, with a reason, and audited.
func TestGrant(t *testing.T) {
	ctx := context.Background()
	p, st, ops, au, use := plugin()
	req := Request{Guild: guild, User: user, Role: role}

	for name, bad := range map[string]Request{
		"a name":         {Guild: "melted", User: user, Role: role},
		"short":          {Guild: guild, User: "12", Role: role},
		"a negative":     {Guild: guild, User: user, Role: "-1000000000000000003"},
		"text in it":     {Guild: guild, User: user, Role: role + " "},
		"nothing at all": {},
	} {
		if err := p.Grant(ctx, "skua", bad); !errors.Is(err, errInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := p.Grant(ctx, "Skua!", req); !errors.Is(err, errInvalid) {
		t.Errorf("a caller that isn't a bird's name: %v", err)
	}
	if err := p.Grant(ctx, "skua", req); !errors.Is(err, errNotAllowed) {
		t.Fatalf("a role nobody allowed: %v", err)
	}
	st.rows[[3]string{guild, "peregrine", role}] = "9"
	if err := p.Grant(ctx, "skua", req); !errors.Is(err, errNotAllowed) {
		t.Errorf("a role allowed to another bird: %v", err)
	}
	st.rows[[3]string{guild, "skua", role}] = "9"

	use[role] = true
	if err := p.Grant(ctx, "skua", req); !errors.Is(err, errNotAllowed) {
		t.Errorf("a role merlin's settings use: %v", err)
	}
	delete(use, role)
	p.roles = fakeRoles{bare: errors.New("the role carries permissions")}
	if err := p.Grant(ctx, "skua", req); !errors.Is(err, errNotAllowed) {
		t.Errorf("a role that gained a permission: %v", err)
	}
	p.roles = fakeRoles{}

	ops.member.User.Bot = true
	if err := p.Grant(ctx, "skua", req); !errors.Is(err, errNotPerson) {
		t.Errorf("a bot: %v", err)
	}
	ops.member.User.Bot = false
	member := ops.member
	ops.member = nil
	if err := p.Grant(ctx, "skua", req); !errors.Is(err, errNotPerson) {
		t.Errorf("someone not in the server: %v", err)
	}
	ops.member = member

	if err := p.Grant(ctx, "skua", req); err != nil || len(ops.added) != 1 || ops.reasons != 1 || len(au.actions) != 1 || au.actions[0] != "fleet:skua fleet.grant" {
		t.Fatalf("granting: %v, added %v, audited %v", err, ops.added, au.actions)
	}
	ops.member.RoleIDs = []snowflake.ID{snowflake.MustParse(role)}
	if err := p.Grant(ctx, "skua", req); err != nil || len(ops.added) != 1 {
		t.Errorf("granting a role already held: %v, added %v", err, ops.added)
	}
	ops.member.RoleIDs = nil

	ops.err = discordguard.ErrPaused
	if err := p.Grant(ctx, "skua", req); !errors.Is(err, errHeld) {
		t.Errorf("a paused server: %v", err)
	}
	ops.err = errors.New("discord fell over")
	if err := p.Grant(ctx, "skua", req); err == nil || errors.Is(err, errHeld) {
		t.Errorf("a failed add: %v", err)
	}
}

// One bird gets perMinute grants in a server a minute.
func TestGrantCap(t *testing.T) {
	p, st, _, _, _ := plugin()
	st.rows[[3]string{guild, "skua", role}] = "9"
	req := Request{Guild: guild, User: user, Role: role}
	for i := range perMinute {
		if err := p.Grant(context.Background(), "skua", req); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	if err := p.Grant(context.Background(), "skua", req); !errors.Is(err, errTooMany) {
		t.Errorf("past the cap: %v", err)
	}
	p.now = func() time.Time { return time.Unix(1_800_000_060, 0) }
	if err := p.Grant(context.Background(), "skua", req); err != nil {
		t.Errorf("the next minute: %v", err)
	}
}

// The link handler decodes strictly and answers in the link's codes.
func TestAnswer(t *testing.T) {
	p, st, ops, _, _ := plugin()
	call := func(from string, body any) error {
		raw, _ := json.Marshal(body)
		_, err := p.Answer(context.Background(), link.Call{From: from, Method: "merlin.grant", Body: raw})
		return err
	}
	good := map[string]string{"guild": guild, "user": user, "role": role}
	for name, c := range map[string]struct {
		body any
		code link.Code
	}{
		"an extra field": {map[string]string{"guild": guild, "user": user, "role": role, "reason": "x"}, link.Invalid},
		"a number":       {map[string]any{"guild": 1, "user": user, "role": role}, link.Invalid},
		"not allowed":    {good, link.Denied},
	} {
		if err := call("skua", c.body); !errors.Is(err, link.Err(c.code)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := p.Answer(context.Background(), link.Call{From: "skua", Body: json.RawMessage(`{"guild":"` + guild + `"} {}`)}); !errors.Is(err, link.Err(link.Invalid)) {
		t.Errorf("two bodies: %v", err)
	}
	st.rows[[3]string{guild, "skua", role}] = "9"
	if err := call("skua", good); err != nil {
		t.Errorf("a good call: %v", err)
	}
	if err := call("skua", map[string]string{"guild": guild, "user": user, "role": "bad"}); !errors.Is(err, link.Err(link.Invalid)) {
		t.Errorf("a bad id: %v", err)
	}
	ops.member.User.Bot = true
	if err := call("skua", good); !errors.Is(err, link.Err(link.Denied)) {
		t.Errorf("a bot: %v", err)
	}
	ops.member.User.Bot = false
	ops.err = discordguard.ErrDryRun
	if err := call("skua", good); !errors.Is(err, link.Err(link.Busy)) {
		t.Errorf("dry run: %v", err)
	}
	ops.err = errors.New("boom")
	if err := call("skua", good); err == nil || errors.Is(err, link.Err(link.Denied)) || errors.Is(err, link.Err(link.Busy)) {
		t.Errorf("a failure: %v", err)
	}
}

// fleetCommand runs /fleet sub with bird and role.
func fleetCommand(t *testing.T, sub, bird, roleID string) (*core.Ix, *paneltest.Wire) {
	return paneltest.IxCommand(t, "9", "fleet", func(p map[string]any) {
		var opts []any
		if bird != "" {
			opts = append(opts, map[string]any{"name": "bird", "type": 3, "value": bird}, map[string]any{"name": "role", "type": 8, "value": roleID})
		}
		p["data"].(map[string]any)["options"] = []any{map[string]any{"name": sub, "type": 1, "options": opts}}
	})
}

// Admins allow, list and deny; allow refuses a role merlin uses or one
// that isn't bare, and every change is audited.
func TestCommands(t *testing.T) {
	ctx := context.Background()
	p, st, _, au, use := plugin()
	g := paneltest.IxGuild

	ix, w := fleetCommand(t, "allow", "Skua!", role)
	p.handleAllow(ctx, ix)
	if len(st.rows) != 0 || !strings.Contains(w.All(), "Not a bird") {
		t.Errorf("allowing a bad name: %s", w.All())
	}
	use[role] = true
	ix, w = fleetCommand(t, "allow", "skua", role)
	p.handleAllow(ctx, ix)
	if len(st.rows) != 0 || !strings.Contains(w.All(), "in use") {
		t.Errorf("allowing a used role: %s", w.All())
	}
	delete(use, role)
	p.roles = fakeRoles{bare: core.ErrForbidden{Reason: "the role carries permissions"}}
	ix, w = fleetCommand(t, "allow", "skua", role)
	p.handleAllow(ctx, ix)
	if len(st.rows) != 0 || !strings.Contains(w.All(), "carries permissions") {
		t.Errorf("allowing a role with permissions: %s", w.All())
	}
	p.roles = fakeRoles{}
	ix, _ = fleetCommand(t, "allow", "skua", role)
	p.handleAllow(ctx, ix)
	if ok, _ := st.Allowed(ctx, g, "skua", role); !ok || len(au.actions) != 1 {
		t.Fatalf("allowing: %v %v", st.rows, au.actions)
	}

	ix, w = fleetCommand(t, "list", "", "")
	p.handleList(ctx, ix)
	if !strings.Contains(w.All(), "skua may ask for") {
		t.Errorf("the list: %s", w.All())
	}
	ix, w = fleetCommand(t, "deny", "skua", other)
	p.handleDeny(ctx, ix)
	if !strings.Contains(w.All(), "Nothing to take back") {
		t.Errorf("denying what wasn't allowed: %s", w.All())
	}
	ix, _ = fleetCommand(t, "deny", "skua", role)
	p.handleDeny(ctx, ix)
	if ok, _ := st.Allowed(ctx, g, "skua", role); ok || len(au.actions) != 2 {
		t.Errorf("denying: %v %v", st.rows, au.actions)
	}
	ix, w = fleetCommand(t, "list", "", "")
	p.handleList(ctx, ix)
	if !strings.Contains(w.All(), "no bird may ask") {
		t.Errorf("the empty list: %s", w.All())
	}
	if p.Name() != "fleet" || p.Help().About == "" || p.Start(ctx) != nil || p.Shutdown(ctx) != nil {
		t.Error("the plugin's basics")
	}
}

type brokenStore struct{}

var errDB = errors.New("db down")

func (brokenStore) Allow(context.Context, string, string, string, string) error { return errDB }
func (brokenStore) Deny(context.Context, string, string, string) (bool, error)  { return false, errDB }
func (brokenStore) Allowed(context.Context, string, string, string) (bool, error) {
	return false, errDB
}
func (brokenStore) List(context.Context, string) ([]Entry, error) { return nil, errDB }

// A store that fails refuses the grant and says so on every command,
// never granting or claiming a change.
func TestStoreFails(t *testing.T) {
	ctx := context.Background()
	p, _, ops, _, _ := plugin()
	p.store = brokenStore{}
	if err := p.Grant(ctx, "skua", Request{Guild: guild, User: user, Role: role}); !errors.Is(err, errDB) || len(ops.added) != 0 {
		t.Errorf("granting on a broken store: %v %v", err, ops.added)
	}
	for _, sub := range []string{"allow", "deny", "list"} {
		bird := "skua"
		if sub == "list" {
			bird = ""
		}
		ix, w := fleetCommand(t, sub, bird, role)
		map[string]func(context.Context, *core.Ix){"allow": p.handleAllow, "deny": p.handleDeny, "list": p.handleList}[sub](ctx, ix)
		if !strings.Contains(w.All(), "db down") {
			t.Errorf("%s on a broken store: %s", sub, w.All())
		}
	}
}
