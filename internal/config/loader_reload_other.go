//go:build !unix

package config

import "context"

// Watch is a no-op everywhere without SIGHUP, which is Windows (the dev
// machine) and js/wasm (cmd/lab). The production target is Linux, see the
// Dockerfile, and the unix build carries the real implementation.
//
// The pair is tagged unix and !unix rather than windows and !windows so that
// every platform gets exactly one definition. Under the old tags js/wasm
// matched the SIGHUP file and failed to compile, which is what a browser
// build first ran into.
//
// Named _other rather than _windows for the same reason: a _windows suffix
// carries an implicit GOOS constraint that ANDs with the explicit one, so the
// file would have been excluded on js/wasm no matter what the tag said, and
// that platform would have had no definition at all.
func (l *Loader) Watch(ctx context.Context) {}
