package core

import (
	"cmp"
	"slices"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// Overwrite is one permission overwrite as a value a module can edit in
// place. disgo's are an interface over two struct types, which is right
// for the wire and awkward for a merge.
type Overwrite struct {
	Type        discord.PermissionOverwriteType
	ID          snowflake.ID
	Allow, Deny discord.Permissions
}

// Overwrites is ows as Overwrite values.
func Overwrites(ows []discord.PermissionOverwrite) []Overwrite {
	out := make([]Overwrite, len(ows))
	for i, ow := range ows {
		switch o := ow.(type) {
		case discord.RolePermissionOverwrite:
			out[i] = Overwrite{discord.PermissionOverwriteTypeRole, o.RoleID, o.Allow, o.Deny}
		case discord.MemberPermissionOverwrite:
			out[i] = Overwrite{discord.PermissionOverwriteTypeMember, o.UserID, o.Allow, o.Deny}
		default:
			out[i] = Overwrite{Type: ow.Type(), ID: ow.ID()}
		}
	}
	return out
}

// Disgo is ows as disgo's overwrites, ready to send.
func Disgo(ows []Overwrite) []discord.PermissionOverwrite {
	out := make([]discord.PermissionOverwrite, len(ows))
	for i, o := range ows {
		if o.Type == discord.PermissionOverwriteTypeMember {
			out[i] = discord.MemberPermissionOverwrite{UserID: o.ID, Allow: o.Allow, Deny: o.Deny}
		} else {
			out[i] = discord.RolePermissionOverwrite{RoleID: o.ID, Allow: o.Allow, Deny: o.Deny}
		}
	}
	return out
}

// SortOverwrites orders ows by type, then ID, in place, and returns it.
func SortOverwrites(ows []Overwrite) []Overwrite {
	slices.SortFunc(ows, func(a, b Overwrite) int {
		return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Allow, b.Allow), cmp.Compare(a.Deny, b.Deny))
	})
	return ows
}

// SameOverwrites compares two lists ignoring order: Discord returns a
// channel's in whatever order it likes, and every permission write lands in
// the guild's audit log, so a module writes only when this says they differ.
func SameOverwrites(a, b []discord.PermissionOverwrite) bool {
	return slices.Equal(SortOverwrites(Overwrites(a)), SortOverwrites(Overwrites(b)))
}
