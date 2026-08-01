//go:build windows

package config

import "context"

// Watch is a no-op on Windows: there is no SIGHUP equivalent, and the
// production target is Linux (see Dockerfile). This stub exists solely so
// go build/go vet succeed on a Windows dev machine.
func (l *Loader) Watch(ctx context.Context) {}
