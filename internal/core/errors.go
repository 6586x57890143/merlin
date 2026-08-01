package core

// ErrForbidden is returned by permission checks when a privileged action is
// denied at any authorization layer. Handlers should surface this to the
// user as a generic "not allowed" without leaking which layer denied it.
type ErrForbidden struct{ Reason string }

func (e ErrForbidden) Error() string { return "forbidden: " + e.Reason }
