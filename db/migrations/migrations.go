// Package migrations embeds the SQL migrations into the binary.
//
// See docs/design/03-persistence.md section 9. Embedding rather than shipping
// a goose CLI means migrations travel inside the image and cannot drift from
// the code that expects them.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed postgres/*.sql sqlite/*.sql
var files embed.FS

// FS returns the migrations for a driver, rooted so goose sees bare filenames.
func FS(driver string) (fs.FS, error) {
	return fs.Sub(files, driver)
}
