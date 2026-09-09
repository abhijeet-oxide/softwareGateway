// Package deploy holds no Go code. This file exists for one assertion that
// nothing else in the repository can make.
package deploy

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// TestScriptsAreLFAndExecutable guards the other fact that breaks every build
// on Windows and names the wrong thing when it does.
//
// A shell script checked out with CRLF has a shebang ending `#!/bin/sh\r`. The
// kernel takes that literally, looks for a program named `/bin/sh\r`, does not
// find one, and reports:
//
//	exec /docker-entrypoint.sh: no such file or directory
//
// The script is present and readable. What is missing is the interpreter, and
// nothing in that message says so - which is why this is worth a test rather
// than a paragraph somebody reads afterwards.
//
// .gitattributes prevents it at checkout and the Dockerfiles strip it at build,
// so this is the third line of defence: it catches a file committed with CRLF
// by an editor that ignored both, before it reaches an image.
func TestScriptsAreLFAndExecutable(t *testing.T) {
	// Every shell script this repository ships, wherever it lives. Kept as a
	// walk rather than a list so a script added tomorrow is covered without
	// anybody remembering this file exists.
	root := ".."
	var scripts []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "bin":
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".sh") {
			scripts = append(scripts, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(scripts) == 0 {
		t.Fatal("found no shell scripts to check, which means this test is not testing anything")
	}

	for _, path := range scripts {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if bytes.Contains(body, []byte("\r\n")) {
			t.Errorf("%s has CRLF line endings. A container will fail to start with "+
				"\"exec %s: no such file or directory\", which names the script and means "+
				"the interpreter. Fix: git add --renormalize . (see .gitattributes)",
				path, filepath.Base(path))
		}
		if !bytes.HasPrefix(body, []byte("#!")) {
			t.Errorf("%s has no shebang, so how it runs depends on who invokes it", path)
		}
	}
}

// TestSeederDoesNotImportHumans guards the fault that made every SSO sign-in on
// a correctly configured stack come back "this account is not recognised".
//
// `POST /management/v1/users/human/_import` accepts `isEmailVerified: true`,
// answers 200, and - when the request carries no password - stores the address
// UNVERIFIED and leaves the account in USER_STATE_INITIAL. Nothing says so. The
// seeder deliberately sets no password when SSO is configured, because the
// person signs in through the identity provider, so every human it created was
// uninitialised with an unverified address.
//
// ZITADEL auto-links an arriving external identity to an existing user by
// VERIFIED address on an ACTIVE account. None of those accounts qualified, every
// sign-in was refused as Errors.User.NotFound, and the seeder's own report
// listed the address as a way in. USER_STATE_INITIAL is also a dead end: the
// address cannot be corrected, cannot be verified, and no password can be set
// on it - all three answer `User is not yet initialized`.
//
// `POST /v2/users/human` honours the flag. This test is here because the two
// endpoints look interchangeable, differ in nothing a reviewer can see, and the
// failure they produce points at the identity provider rather than at the line
// that caused it.
func TestSeederDoesNotImportHumans(t *testing.T) {
	body, err := os.ReadFile("zitadel/bootstrap.mjs")
	if err != nil {
		t.Fatalf("bootstrap.mjs: %v", err)
	}
	// The quoted form, so the comment explaining why it is gone does not
	// trip its own test.
	if bytes.Contains(body, []byte("'/management/v1/users/human/_import'")) {
		t.Error("deploy/zitadel/bootstrap.mjs calls /management/v1/users/human/_import. " +
			"That endpoint silently ignores isEmailVerified when no password is sent, " +
			"leaving the account in USER_STATE_INITIAL with an unverified address, which " +
			"no SSO sign-in can ever be matched to. Use POST /v2/users/human.")
	}
	if !bytes.Contains(body, []byte("'/v2/users/human'")) {
		t.Error("deploy/zitadel/bootstrap.mjs no longer creates people through " +
			"POST /v2/users/human, which is the only create path that leaves an " +
			"account active with a verified address and no password.")
	}
}
