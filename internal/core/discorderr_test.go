package core

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/disgoorg/disgo/rest"
)

// restErr is a Discord error as disgo's REST client returns it; code 0 is
// one whose body had no code.
func restErr(status int, code rest.JSONErrorCode) error {
	return &rest.Error{Response: &http.Response{StatusCode: status}, Code: code}
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
		{"unknown channel", restErr(http.StatusNotFound, rest.JSONErrorCodeUnknownChannel), true},
		{"unknown member", restErr(http.StatusNotFound, rest.JSONErrorCodeUnknownMember), true},
		{"unknown role", restErr(http.StatusNotFound, rest.JSONErrorCodeUnknownRole), true},
		{"unknown message", restErr(http.StatusNotFound, rest.JSONErrorCodeUnknownMessage), true},
		{"404 with no code", restErr(http.StatusNotFound, 0), true},
		{"wrapped unknown channel", fmt.Errorf("fetch: %w", restErr(http.StatusNotFound, rest.JSONErrorCodeUnknownChannel)), true},

		{"rate limited", restErr(http.StatusTooManyRequests, 0), false},
		{"server error", restErr(http.StatusInternalServerError, 0), false},
		{"missing permissions", restErr(http.StatusForbidden, rest.JSONErrorCodeLackPermissionsToPerformAction), false},
		{"missing access", restErr(http.StatusForbidden, rest.JSONErrorCodeMissingAccess), false},
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
	if IsUnknownResource(restErr(http.StatusForbidden, rest.JSONErrorCodeLackPermissionsToPerformAction)) {
		t.Error("a permission denial must not be treated as the resource being gone")
	}
}
