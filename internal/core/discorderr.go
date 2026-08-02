package core

import (
	"errors"
	"net/http"

	"github.com/bwmarrin/discordgo"
)

// unknownResourceCodeMin/Max bound Discord's "Unknown <thing>" JSON error
// codes (10001 Unknown Account through 10087 and up) — the whole 100xx band
// means "this ID doesn't exist," never a transient condition.
const (
	unknownResourceCodeMin = 10000
	unknownResourceCodeMax = 10999
)

// IsUnknownResource reports whether err is Discord telling us the thing we
// asked about doesn't exist (a deleted channel, a member who left, a removed
// role) as opposed to a transient failure — a rate limit, a 5xx, a dropped
// connection.
//
// That distinction is load-bearing wherever this bot cleans up its own
// tracking state: treating "gone" as a reason to stop tracking is correct and
// self-healing, but treating a transient blip the same way silently drops
// work that was still pending — an archived channel never swept past its
// retention window, a jailed member never released. Callers must fail and
// retry on anything this returns false for.
func IsUnknownResource(err error) bool {
	var restErr *discordgo.RESTError
	if !errors.As(err, &restErr) {
		return false
	}
	if restErr.Message != nil && restErr.Message.Code >= unknownResourceCodeMin && restErr.Message.Code <= unknownResourceCodeMax {
		return true
	}
	// No parsed JSON body to go on (an empty or malformed error payload):
	// the status alone still distinguishes "no such thing" from a retryable
	// failure.
	return restErr.Message == nil && restErr.Response != nil && restErr.Response.StatusCode == http.StatusNotFound
}
