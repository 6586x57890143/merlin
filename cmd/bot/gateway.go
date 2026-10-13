package main

import (
	"encoding/json"

	"github.com/disgoorg/snowflake/v2"
)

// legacy is v in discordgo's type T, through JSON, for a plugin entry point
// that still takes discordgo's types. Only for events too rare for the
// round trip to matter (a member joining, an audit-log entry, a role edit, a
// new thread); each goes when its plugin moves to disgo. nil if v does not
// convert.
func legacy[T any](v any) *T {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var t T
	if err := json.Unmarshal(b, &t); err != nil {
		return nil
	}
	return &t
}

// ids is snowflakes as merlin's string IDs.
func ids(in []snowflake.ID) []string {
	out := make([]string, len(in))
	for i, id := range in {
		out[i] = id.String()
	}
	return out
}
