// Package tlscompat applies process-wide TLS relaxations from configuration.
//
// It exists because some of what operators need cannot be expressed per
// connection. Go's stricter X.509 rules are governed by GODEBUG settings, which
// crypto/x509 reads from the process environment; there is no tls.Config field
// for them and no way to scope them to one registry. Rather than hide that in
// wiring code, it is one package with one job, and the blast radius is stated
// in the log line it emits.
//
// See docs/design/02-configuration.md §9.
package tlscompat

import (
	"log/slog"
	"os"
	"strings"
)

// Setting is one GODEBUG key we know how to turn on.
type Setting struct {
	// Key is the GODEBUG name, e.g. "x509negativeserial".
	Key string
	// Why is logged with the warning, so the log says what was relaxed and what
	// it costs rather than only that something was.
	Why string
}

// NegativeSerial accepts certificates with a negative serial number.
//
// crypto/x509 has rejected these since Go 1.23. The rejection happens while
// PARSING, before verification, which is why skipping verification does not
// help - measured on Go 1.25.7, where InsecureSkipVerify alone fails with the
// identical "tls: failed to parse certificate from server" message.
var NegativeSerial = Setting{
	Key: "x509negativeserial",
	Why: "certificates with a negative serial number will be accepted; they violate " +
		"RFC 5280 §4.1.2.2 but are otherwise verified normally",
}

// Options selects which relaxations to apply.
type Options struct {
	AllowNegativeSerialNumbers bool
}

// Apply sets the GODEBUG entries the options ask for.
//
// Returns the settings it enabled, for tests and for the startup banner.
//
// Call this before any TLS connection is made. crypto/x509 re-reads the value,
// so a late call is not fatal, but a connection made before it would fail for
// the very reason the setting exists.
func Apply(opts Options, log *slog.Logger) []Setting {
	if log == nil {
		log = slog.Default()
	}

	var applied []Setting
	if opts.AllowNegativeSerialNumbers {
		applied = append(applied, NegativeSerial)
	}
	if len(applied) == 0 {
		return nil
	}

	current := os.Getenv("GODEBUG")
	next := current
	for _, s := range applied {
		next = mergeGODEBUG(next, s.Key, "1")
	}

	if err := os.Setenv("GODEBUG", next); err != nil {
		// Setenv failing is close to impossible, but silently continuing would
		// leave the operator with the failure they configured this to fix and
		// no clue why the setting did nothing.
		log.Error("tls: could not apply GODEBUG relaxations; the setting has NOT taken effect",
			"error", err, "godebug", next)
		return nil
	}

	for _, s := range applied {
		log.Warn("tls: RELAXED X.509 PARSING FOR THE WHOLE PROCESS - this applies to every "+
			"registry, every Sigstore call and every outbound connection, not just the "+
			"repository that needed it",
			"godebug", s.Key+"=1",
			"effect", s.Why,
			"setting", "tls.allowNegativeSerialNumbers")
	}
	return applied
}

// mergeGODEBUG sets key=value in a GODEBUG string, replacing any existing entry
// for that key and preserving everything else.
//
// Appending rather than assigning matters: GODEBUG is commonly already set in a
// container image or a Deployment - for http2debug, for a panicnil or
// tlsmaxrsasize migration - and clobbering it would silently undo whatever it
// was doing. That is the kind of breakage nobody connects back to this setting.
func mergeGODEBUG(current, key, value string) string {
	entry := key + "=" + value
	if current == "" {
		return entry
	}

	parts := strings.Split(current, ",")
	out := make([]string, 0, len(parts)+1)
	replaced := false
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		if k, _, found := strings.Cut(trimmed, "="); found && k == key {
			out = append(out, entry)
			replaced = true
			continue
		}
		out = append(out, trimmed)
	}
	if !replaced {
		out = append(out, entry)
	}
	return strings.Join(out, ",")
}
