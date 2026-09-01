package compliance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Loading policy packs from disk.
//
// # Why a directory and not the database
//
// A check that can fail a release is a control. Controls belong in Git, where
// they are reviewed, diffed and attributed - not in a table where somebody can
// change a severity at 2am with nobody the wiser. This is the same argument
// that keeps product configuration read-only over the API, and it is why there
// is no waiver UI either.
//
// The mechanism is the one product configuration already uses: a mounted
// directory, discovered on start and re-read on change, with the identical code
// path against a plain directory in local development.
//
// # Why a broken pack does not stop the others
//
// Fail-closed per pack, never per catalogue. One pack with a typo must not take
// the shipped baseline down, and it must not vanish either: it is recorded with
// its error, surfaced in the API and on the policy page, and every check it
// owns reports `error`. A check that disappears looks exactly like a check that
// passed, and that is the failure this whole package exists to prevent.

// Loader discovers and compiles policy packs.
type Loader struct {
	// Compiler turns a declared check into a Program.
	Compiler Compiler
	// Builtins are checks implemented in Go, by check ID. A pack entry that
	// declares `engine: builtin` is bound to the registered implementation, and
	// one with no implementation is a load error for that pack rather than a
	// check that silently does nothing.
	Builtins map[string]Program
}

// Load reads every pack under the given directories and returns a catalogue.
//
// The returned catalogue is always usable: directories that do not exist are
// skipped, packs that fail are recorded, and the built-in baseline - which the
// caller registers first - is untouched by any of it.
func (l *Loader) Load(dirs ...string) (*Catalog, error) {
	cat := NewCatalog()

	// prefix -> pack that owns it. This is what makes a check ID globally
	// unique with no central registry: the second pack claiming a prefix is
	// rejected, named against the first.
	owners := make(map[string]string)

	files, err := discover(dirs)
	if err != nil {
		return cat, err
	}

	hash := sha256.New()
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // operator-supplied policy path
		if err != nil {
			cat.AddPackStatus(PackStatus{Name: filepath.Base(f), Path: f,
				Errors: []string{fmt.Sprintf("read: %v", err)}})
			continue
		}
		// Every byte of every loaded file, in path order, so "which rulebook
		// produced this report" has an answer a year later. Path included, so
		// moving a check between files is a different bundle - because it is a
		// different catalogue if the move changed load order.
		hash.Write([]byte(f))
		hash.Write(b)

		cat.AddPackStatus(l.loadPack(b, f, owners, cat))
	}
	cat.BundleDigest = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return cat, nil
}

// loadPack parses, validates and compiles one manifest.
//
// It reports EVERY problem it finds rather than the first. A pack is edited by
// a person who then waits for a reload; one error per attempt turns a
// five-mistake manifest into five round trips and teaches them nothing about
// the schema.
func (l *Loader) loadPack(b []byte, path string, owners map[string]string, cat *Catalog) PackStatus {
	status := PackStatus{Name: filepath.Base(path), Path: path}

	var pack Pack
	// Strict, so `severtiy: block` is an error rather than a check that quietly
	// has no severity. A silently-ignored field is a control that does not do
	// what its author believes it does.
	if err := yaml.UnmarshalStrict(b, &pack); err != nil {
		status.Errors = append(status.Errors, fmt.Sprintf("parse: %v", err))
		return status
	}
	if pack.Metadata.Name != "" {
		status.Name = pack.Metadata.Name
	}
	status.Version = pack.Metadata.Version
	status.Description = pack.Metadata.Description
	status.Maintainer = pack.Metadata.Maintainer
	status.Reference = pack.Metadata.Reference
	status.Prefixes = pack.OwnedPrefixes()

	for _, err := range pack.Validate() {
		status.Errors = append(status.Errors, err.Error())
	}
	for _, prefix := range pack.OwnedPrefixes() {
		if prev, taken := owners[prefix]; taken {
			status.Errors = append(status.Errors,
				fmt.Sprintf("prefix %q is already owned by pack %q; two packs owning one prefix means a check ID is ambiguous", prefix, prev))
		}
	}
	if len(status.Errors) > 0 {
		return status
	}

	// Compile everything before registering anything. A pack is atomic: half a
	// pack loaded is a catalogue nobody can reason about, and the half that
	// failed is invisible.
	type compiled struct {
		check Check
		prog  Program
	}
	batch := make([]compiled, 0, len(pack.Spec.Checks))
	seen := make(map[string]bool, len(pack.Spec.Checks))

	for i, check := range pack.Spec.Checks {
		check.Pack = status.Name
		where := fmt.Sprintf("checks[%d]", i)
		if check.ID != "" {
			where = check.ID
		}

		for _, err := range check.Validate() {
			status.Errors = append(status.Errors, fmt.Sprintf("%s: %v", where, err))
		}
		if check.ID == "" {
			continue
		}
		if seen[check.ID] {
			status.Errors = append(status.Errors, fmt.Sprintf("%s: declared twice in this pack", check.ID))
			continue
		}
		seen[check.ID] = true

		if !contains(pack.OwnedPrefixes(), check.Prefix()) {
			status.Errors = append(status.Errors, fmt.Sprintf(
				"%s: prefix %q is not owned by this pack (it owns %s)",
				where, check.Prefix(), strings.Join(pack.OwnedPrefixes(), ", ")))
			continue
		}
		if _, taken := cat.Check(check.ID); taken {
			status.Errors = append(status.Errors, fmt.Sprintf("%s: already defined by another pack", where))
			continue
		}
		if check.Deprecated {
			batch = append(batch, compiled{check: check})
			continue
		}

		prog, err := l.compile(check)
		if err != nil {
			status.Errors = append(status.Errors, fmt.Sprintf("%s: %v", where, err))
			continue
		}
		batch = append(batch, compiled{check: check, prog: prog})
	}

	if len(status.Errors) > 0 {
		return status
	}
	for _, c := range batch {
		if err := cat.Add(c.check, c.prog); err != nil {
			status.Errors = append(status.Errors, err.Error())
		}
	}
	if len(status.Errors) > 0 {
		return status
	}
	for _, prefix := range pack.OwnedPrefixes() {
		owners[prefix] = status.Name
	}
	status.Checks = len(batch)
	return status
}

func (l *Loader) compile(check Check) (Program, error) {
	if check.EngineName() == EngineBuiltin {
		prog, ok := l.Builtins[check.ID]
		if !ok {
			// Declared as builtin with nothing behind it. Rejected rather than
			// registered, because a check present in the catalogue and absent
			// from the run is worse than one that is missing from both.
			return nil, fmt.Errorf("declares engine: builtin but no implementation is registered for this ID")
		}
		return prog, nil
	}
	if l.Compiler == nil {
		return nil, fmt.Errorf("no expression compiler is configured")
	}
	return l.Compiler.Compile(check)
}

// discover lists candidate manifests under the given directories.
//
// A missing directory is not an error: the policy mount is optional, and a
// deployment with no packs of its own still gets the built-in baseline. Files
// that are not YAML are skipped, because the same directory holds waiver files
// and READMEs and a loader that failed on them would make it unusable for
// anything else.
func discover(dirs []string) ([]string, error) {
	var files []string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				// testdata holds fixtures - charts and expectations - not
				// packs. Walking into it would try to load a chart as a
				// manifest and report a pack that does not exist as broken.
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml":
				files = append(files, path)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return files, fmt.Errorf("scanning policy directory %s: %w", dir, err)
		}
	}
	// Deterministic order, so the bundle digest and any duplicate-prefix
	// reporting are stable across restarts and across machines.
	sort.Strings(files)
	return files, nil
}
