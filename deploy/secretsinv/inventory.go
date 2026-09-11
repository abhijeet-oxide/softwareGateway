// Package secretsinv reads config/secrets/secrets.yaml - the list of
// credentials this deployment needs.
//
// It exists because three things need the same list and none of them should
// restate it:
//
//	the chart      renders one VaultStaticSecret or SecretProviderClass per
//	               entry (deploy/charts/software-gateway/templates/secrets.yaml)
//	a developer    needs the directory layout under config/secrets/local, and
//	               guessing at paths is how a credential ends up one directory
//	               too deep and reported as missing
//	CI             fails the pull request when a product references a secret
//	               nobody declared - the same shape of check as "every product
//	               has an owner"
//
// The inventory carries no values and cannot: there is nowhere in this struct
// to put one.
package secretsinv

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Path is the inventory's location relative to the repository root.
const Path = "config/secrets/secrets.yaml"

// Entry is one credential: what it is called, what it contains, and where the
// backend should look for it. `Path` is a LEAF - the root is a per-environment
// value (secrets.vault.pathPrefix, secrets.azure.keyvaultName), because lab and
// production read from different mounts and this file is shared by both.
type Entry struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Keys        []string `json:"keys"`
	Path        string   `json:"path"`
}

// Inventory is the whole document.
type Inventory struct {
	APIVersion string  `json:"apiVersion"`
	Kind       string  `json:"kind"`
	Secrets    []Entry `json:"secrets"`
}

// Names returns every declared secret name.
func (i Inventory) Names() []string {
	names := make([]string, 0, len(i.Secrets))
	for _, e := range i.Secrets {
		names = append(names, e.Name)
	}
	return names
}

// Has reports whether name is declared.
func (i Inventory) Has(name string) bool {
	for _, e := range i.Secrets {
		if e.Name == name {
			return true
		}
	}
	return false
}

// Load reads the inventory from a repository root.
func Load(root string) (Inventory, error) {
	var inv Inventory
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(Path)))
	if err != nil {
		return inv, fmt.Errorf("read %s: %w", Path, err)
	}
	if err := yaml.Unmarshal(b, &inv); err != nil {
		return inv, fmt.Errorf("parse %s: %w", Path, err)
	}
	for n, e := range inv.Secrets {
		switch {
		case e.Name == "":
			return inv, fmt.Errorf("%s: entry %d has no name", Path, n)
		case len(e.Keys) == 0:
			return inv, fmt.Errorf("%s: %q declares no keys - the keys ARE the file names a "+
				"process reads, so an entry without them produces an empty directory", Path, e.Name)
		case e.Path == "":
			return inv, fmt.Errorf("%s: %q has no path - the backend would have nothing to look up", Path, e.Name)
		}
	}
	return inv, nil
}

// LocalDir is where the hand-filled copy lives. Never committed.
const LocalDir = "config/secrets/local"

// Scaffold writes the empty local layout: one directory per secret, one file
// per key, and nothing in them.
//
// EMPTY FILES RATHER THAN PLACEHOLDER TEXT, and that is the point of it. A file
// containing `CHANGEME` is a credential the registry rejects with a 401 that
// names nothing; an empty one is reported at load, by path, as the missing
// value it is. Existing files are never touched.
func Scaffold(root string) ([]string, error) {
	inv, err := Load(root)
	if err != nil {
		return nil, err
	}
	var created []string
	for _, e := range inv.Secrets {
		dir := filepath.Join(root, filepath.FromSlash(LocalDir), e.Name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return created, err
		}
		for _, k := range e.Keys {
			f := filepath.Join(dir, k)
			if _, err := os.Stat(f); err == nil {
				continue
			}
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				return created, err
			}
			created = append(created, filepath.ToSlash(filepath.Join(LocalDir, e.Name, k)))
		}
	}
	return created, nil
}
