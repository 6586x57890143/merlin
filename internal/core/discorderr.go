package core

import (
	"errors"
	"net/http"

	"github.com/disgoorg/disgo/rest"
)

// unknownResourceCodeMin/Max bound Discord's "Unknown <thing>" JSON error
// codes (10001 Unknown Account through 10087 and up). The whole 100xx band
// means "this ID doesn't exist," never a transient condition.
const (
	unknownResourceCodeMin = 10000
	unknownResourceCodeMax = 10999
)

// IsUnknownResource reports whether err is Discord telling us the thing we
// asked about doesn't exist (a deleted channel, a member who left, a removed
// role) as opposed to a transient failure: a rate limit, a 5xx, a dropped
// connection.
//
// That distinction is load-bearing wherever this bot cleans up its own
// tracking state: treating "gone" as a reason to stop tracking is correct and
// self-healing, but treating a transient blip the same way silently drops
// work that was still pending: an archived channel never swept past its
// retention window, a jailed member never released. Callers must fail and
// retry on anything this returns false for.
func IsUnknownResource(err error) bool {
	if re, ok := errors.AsType[*rest.Error](err); ok {
		if code := int(re.Code); code >= unknownResourceCodeMin && code <= unknownResourceCodeMax {
			return true
		}
		// No code to go on (an empty or malformed error body): the status
		// alone still tells "no such thing" from a retryable failure.
		return re.Code == 0 && re.Response != nil && re.Response.StatusCode == http.StatusNotFound
	}
	return false
}

// HasDiscordErrorCode reports whether err is a Discord REST error carrying
// exactly the given code (rest.JSONErrorCodeUnknownRole, ...).
//
// Use this, not IsUnknownResource, when the reaction depends on *which* thing
// is missing rather than on "something is missing." One API call can report
// several different absences (GuildMemberEdit answers Unknown Member for a
// target who left and Unknown Role for a deleted role), and a caller that
// treats them alike acts on the wrong one.
func HasDiscordErrorCode(err error, code int) bool {
	re, ok := errors.AsType[*rest.Error](err)
	return ok && int(re.Code) == code
}
