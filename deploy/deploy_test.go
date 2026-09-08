// Package deploy holds no Go code. This file exists for one assertion that
// nothing else in the repository can make.
package deploy

import (
	"os"
	"testing"
)

// TestBuildSecretDefaultsAreEmpty guards a fact whose violation breaks every
// image build on Windows, and does it in a way no reviewer would spot.
//
// A build secret's default file is mounted when no credentials are configured.
// podman's remote client - podman machine, so every Windows and macOS host -
// cannot pass a secret to the server over the wire: it copies the file INTO
// THE BUILD CONTEXT, keeps the handle open, and tars the context. On Windows
// the size in the tar header comes from the directory entry, which still reads
// zero while podman's writes sit unflushed, so the copy delivers more bytes
// than the header promised and archive/tar refuses it:
//
//	archive/tar: write too long
//	Error: Post "http://d/v5.5.2/libpod/build?...": io: read/write on closed pipe
//
// A zero byte secret is immune, because zero is what the header promised.
//
// Both files previously carried a comment saying "Intentionally empty" while
// being 215 and 423 bytes, which is exactly the trap: the file documents that
// it is empty, is not, and every build on Windows fails on an error that names
// neither the file nor podman. See containers/podman#26914, #17899, #23815.
func TestBuildSecretDefaultsAreEmpty(t *testing.T) {
	// Every file docker-compose.yml names as a `secrets:` source by default.
	for _, path := range []string{"go/netrc.default", "npm/npmrc.default"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s: %v (docker-compose.yml declares it as a build secret source)", path, err)
			continue
		}
		if info.Size() != 0 {
			t.Errorf("%s is %d bytes and must be 0. Anything to say about it belongs in "+
				"the README beside it: a non-empty default breaks every podman build on "+
				"Windows. See this test's comment.", path, info.Size())
		}
	}
}
