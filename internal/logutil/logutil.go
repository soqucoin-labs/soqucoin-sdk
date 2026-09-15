// Package logutil holds the one rule every package applies to an injected
// logger: nil means discard.
package logutil

import "log/slog"

// Or returns l, or a logger that discards everything when l is nil, so a
// package never has to check for nil before it logs.
func Or(l *slog.Logger) *slog.Logger {
	if l != nil {
		return l
	}
	return slog.New(slog.DiscardHandler)
}
