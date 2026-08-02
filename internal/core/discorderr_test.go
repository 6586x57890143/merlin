package core

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func restErr(status int, code int) *discordgo.RESTError {
	e := &discordgo.RESTError{Response: &http.Response{StatusCode: status}}
	if code != 0 {
		e.Message = &discordgo.APIErrorMessage{Code: code, Message: "test"}
	}
	return e
}

// TestIsUnknownResource pins down the distinction every cleanup path in this
// bot branches on: "this thing is gone, stop tracking it" versus "try again
// later." Getting it backwards either strands work forever (a jailed member
// never released, an archive never swept) or drops it silently, and neither
// failure is visible from the outside.
func TestIsUnknownResource(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"unknown channel", restErr(http.StatusNotFound, discordgo.ErrCodeUnknownChannel), true},
		{"unknown member", restErr(http.StatusNotFound, discordgo.ErrCodeUnknownMember), true},
		{"unknown role", restErr(http.StatusNotFound, discordgo.ErrCodeUnknownRole), true},
		{"unknown message", restErr(http.StatusNotFound, discordgo.ErrCodeUnknownMessage), true},
		{"404 with no parsed body", restErr(http.StatusNotFound, 0), true},
		{"wrapped unknown channel", fmt.Errorf("fetch: %w", restErr(http.StatusNotFound, discordgo.ErrCodeUnknownChannel)), true},

		{"rate limited", restErr(http.StatusTooManyRequests, 0), false},
		{"server error", restErr(http.StatusInternalServerError, 0), false},
		{"missing permissions", restErr(http.StatusForbidden, discordgo.ErrCodeMissingPermissions), false},
		{"missing access", restErr(http.StatusForbidden, discordgo.ErrCodeMissingAccess), false},
		{"plain error", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnknownResource(tc.err); got != tc.want {
				t.Errorf("IsUnknownResource(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsUnknownResourceIgnoresPermissionDenials is called out separately
// because it's the tempting wrong answer: a 403 also means "you can't have
// this," but the thing still exists and access may be restored, so the
// tracking row must survive.
func TestIsUnknownResourceIgnoresPermissionDenials(t *testing.T) {
	if IsUnknownResource(restErr(http.StatusForbidden, discordgo.ErrCodeMissingPermissions)) {
		t.Error("a permission denial must not be treated as the resource being gone")
	}
}
