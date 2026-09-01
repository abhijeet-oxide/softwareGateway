package middleware

import (
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// compressibleTypes are the content types worth compressing.
//
// An allowlist rather than "everything", because most of the bytes this
// service moves are already compressed: layer blobs, an xlsx, an export ZIP.
// Running deflate over those spends CPU on both ends to make the body very
// slightly larger.
//
// text/csv is here and not in chi's default list, and it is the one that
// matters most in that set: an export of ninety thousand findings is columns
// of repeated severity words and package names, which is close to the best
// case deflate has.
var compressibleTypes = []string{
	"application/json",
	"application/problem+json",
	"application/xml",
	"text/csv",
	"text/html",
	"text/plain",
	"image/svg+xml",
}

// Compress deflates a response when the caller asked for it.
//
// # Why this exists
//
// Because the API's largest answers are JSON in which almost everything
// repeats. A comparison of two 84,000-finding releases is 119 MB on the wire,
// and it is 119 MB of the same three thousand CVE descriptions, the same
// thirteen image names and the same five severity words written out over and
// over. That body is not slow because the database is slow - it is slow
// because it is 119 MB, and over a link that carries a megabyte a second it
// alone is two minutes of a reader watching a spinner. Compressed it is a few
// megabytes.
//
// The response shapes were made smaller too (see the comparison handler),
// which is the fix that removes the work rather than merely encoding it
// faster. This is the floor under all of them: every listing this service will
// ever add gets it without being asked.
//
// # Position in the chain
//
// Innermost, so the handler writes into the compressor and everything outside
// - logging, metrics, tracing - observes the bytes that actually left. A log
// line claiming 119 MB for a response that put 6 MB on the wire would send
// the next person reading it looking in the wrong place.
//
// Level 5 rather than 9: on this JSON the last four levels buy about two per
// cent for roughly three times the CPU, and this runs on the request path.
func Compress(next http.Handler) http.Handler {
	return middleware.Compress(5, compressibleTypes...)(next)
}
