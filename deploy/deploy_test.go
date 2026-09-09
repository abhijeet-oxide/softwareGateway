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

	"sigs.k8s.io/yaml"
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

// TestSeederInventsNoPersonName guards a small thing that lands on the one
// screen where it is least welcome.
//
// ZITADEL requires both a given and a family name, and this seeder does not
// know anybody's. What it used to write was 'Platform' / 'Administrator' for
// the first administrator and 'User' as a surname for everybody in users.json:
// a fabricated person's name, on a real person's account, shown to them on
// their own profile page under their own initials.
//
// It does not stay fabricated forever - the connector carries isAutoUpdate, so
// the directory's own name replaces it - but NOT on the sign-in that links the
// account, only on the next one. So there is a real window, usually somebody's
// first impression of the product, where whatever the seeder chose is what
// they read.
//
// nameFor is what replaced it: the operator's value, or the name the address
// spells, or the username. All three are true.
func TestSeederInventsNoPersonName(t *testing.T) {
	body, err := os.ReadFile("zitadel/bootstrap.mjs")
	if err != nil {
		t.Fatalf("bootstrap.mjs: %v", err)
	}
	for _, invented := range []string{
		"lastName: 'Administrator'",
		"lastName: 'User'",
		"u.lastName || 'User'",
	} {
		if bytes.Contains(body, []byte(invented)) {
			t.Errorf("deploy/zitadel/bootstrap.mjs writes %q as part of a person's name. "+
				"It does not know their name. Derive one with nameFor, which uses what the "+
				"operator supplied, then what the address spells, then the username.", invented)
		}
	}
	if !bytes.Contains(body, []byte("profile: nameFor(")) {
		t.Error("deploy/zitadel/bootstrap.mjs no longer builds a person's profile through " +
			"nameFor, which is the one place that decides what to write when the name is " +
			"not known.")
	}
}

// TestEveryProductHasAnOwner enforces, on the pull request, the rule the
// seeder enforces at apply time.
//
// A product nobody holds the owner role on is a product whose downloads nobody
// can approve and whose configuration nobody is accountable for. The seeder
// refuses to finish on one, which is correct and also late: by then the change
// is merged and somebody is watching a deployment fail. This is the same check
// against the same two files, run by `go test ./deploy/...`.
//
// It reads the files with a real YAML library on purpose. The seeder parses
// them with a deliberately small reader of its own (deploy/zitadel/bootstrap.mjs
// explains why it cannot take a dependency), so this is also the test that a
// document written here is one that reader can read.
func TestEveryProductHasAnOwner(t *testing.T) {
	var roles struct {
		Tenant  struct{ Roles []string } `json:"tenant"`
		Product struct {
			Roles     []string `json:"roles"`
			OwnerRole string   `json:"ownerRole"`
		} `json:"product"`
	}
	readYAML(t, "../data/access/roles.yaml", &roles)
	if len(roles.Tenant.Roles) == 0 || len(roles.Product.Roles) == 0 {
		t.Fatal("data/access/roles.yaml declares no roles; both tiers are required")
	}
	if roles.Product.OwnerRole == "" {
		t.Skip("data/access/roles.yaml sets no product.ownerRole, so no product needs one")
	}

	var users struct {
		Users    []userEntry `json:"users"`
		APIUsers []userEntry `json:"apiUsers"`
	}
	readYAML(t, "../data/users/users.yaml", &users)

	owned := map[string]bool{}
	for _, u := range append(append([]userEntry{}, users.Users...), users.APIUsers...) {
		for product, granted := range u.Products {
			for _, role := range granted {
				if role == roles.Product.OwnerRole {
					owned[product] = true
				}
			}
		}
	}

	files, err := filepath.Glob("../data/products/*.yaml")
	if err != nil {
		t.Fatalf("glob products: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("found no products to check, which means this test is not testing anything")
	}
	for _, file := range files {
		var doc struct {
			Metadata struct{ Name string } `json:"metadata"`
		}
		readYAML(t, file, &doc)
		if doc.Metadata.Name == "" {
			t.Errorf("%s declares no metadata.name, so nobody can be granted access to it", file)
			continue
		}
		if !owned[doc.Metadata.Name] {
			t.Errorf("product %q (%s) has no owner. Add it to somebody's `products:` mapping in "+
				"data/users/users.yaml with the %q role, or nobody can approve a download for it:"+
				"\n      products:\n        %s: [%s]",
				doc.Metadata.Name, file, roles.Product.OwnerRole,
				doc.Metadata.Name, roles.Product.OwnerRole)
		}
	}
}

// A person or a machine account, as far as this check is concerned.
type userEntry struct {
	Username string              `json:"username"`
	Products map[string][]string `json:"products"`
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if err := yaml.Unmarshal(body, into); err != nil {
		t.Fatalf("%s is not valid YAML: %v", path, err)
	}
}
