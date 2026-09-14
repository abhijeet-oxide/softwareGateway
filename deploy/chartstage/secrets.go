package chartstage

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// A deployment with no credentials operator needs its Secrets created by hand,
// once, before Flux installs anything. Which ones depends on what the instance's
// values file says - so the list is derived from that file rather than written
// down beside it, because a list written down is a list that goes stale the
// first time somebody turns single sign-on on.

// Secret is one Kubernetes Secret that must exist before a release installs.
type Secret struct {
	Namespace string
	Name      string
	// Kind is `generic` or `docker-registry`, matching `kubectl create secret`.
	Kind string
	Keys []string
	// Why names the values that reference it, so a reader can see what stops
	// needing it when they change their mind.
	Why string
	// Command is the whole `kubectl create secret ...` invocation, ready to run.
	Command []string
}

type instanceValues struct {
	Layers struct {
		Database *bool `json:"database"`
	} `json:"layers"`
	Image struct {
		Registry string `json:"registry"`
	} `json:"image"`
	ImagePullSecrets []string `json:"imagePullSecrets"`
	Database         struct {
		ExistingSecret string `json:"existingSecret"`
		Cluster        struct {
			Name *string `json:"name"`
		} `json:"cluster"`
	} `json:"database"`
	Identity struct {
		Enabled   *bool `json:"enabled"`
		Masterkey struct {
			ExistingSecret string `json:"existingSecret"`
			Key            string `json:"key"`
		} `json:"masterkey"`
		RootPassword struct {
			ExistingSecret string `json:"existingSecret"`
			Key            string `json:"key"`
		} `json:"rootPassword"`
		BootstrapAdmin struct {
			ExistingSecret string `json:"existingSecret"`
		} `json:"bootstrapAdmin"`
		SSO struct {
			Issuer          string `json:"issuer"`
			ExistingSecret  string `json:"existingSecret"`
			ClientSecretKey string `json:"clientSecretKey"`
		} `json:"sso"`
	} `json:"identity"`
	Secrets struct {
		Backend string `json:"backend"`
	} `json:"secrets"`
}

// RequiredSecrets reports every Secret an instance needs created by hand,
// in the order it is easiest to create them.
func RequiredSecrets(root, instance string) ([]Secret, error) {
	if err := checkSegment("instance", instance); err != nil {
		return nil, err
	}
	raw, err := InstanceValues(root, instance)
	if err != nil {
		return nil, err
	}
	var v instanceValues
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("parse %s values: %w", instance, err)
	}
	ns, err := instanceNamespace(root, instance)
	if err != nil {
		return nil, err
	}

	// A credentials operator produces everything the inventory names, so only a
	// deployment without one has anything to create.
	byHand := v.Secrets.Backend == "" || v.Secrets.Backend == "none"

	var out []Secret
	add := func(s Secret) {
		s.Namespace = ns
		s.Command = createCommand(s)
		out = append(out, s)
	}

	for _, name := range v.ImagePullSecrets {
		add(Secret{
			Name: name,
			Kind: "docker-registry",
			Keys: []string{".dockerconfigjson"},
			Why:  "imagePullSecrets - the registry the kubelet pulls every image with",
		})
	}

	// The two ZITADEL values that have no safe default, grouped by the Secret
	// they are named in - usually one Secret with two keys.
	if v.Identity.Enabled == nil || *v.Identity.Enabled {
		keys := map[string][]string{}
		note := map[string]string{}
		if s := v.Identity.Masterkey.ExistingSecret; s != "" {
			keys[s] = append(keys[s], orDefault(v.Identity.Masterkey.Key, "masterkey"))
			note[s] = "identity.masterkey"
		}
		if s := v.Identity.RootPassword.ExistingSecret; s != "" {
			keys[s] = append(keys[s], orDefault(v.Identity.RootPassword.Key, "rootPassword"))
			note[s] = strings.TrimPrefix(note[s]+" and identity.rootPassword", " and ")
		}
		if s := v.Identity.BootstrapAdmin.ExistingSecret; s != "" {
			keys[s] = append(keys[s], "adminPassword")
			note[s] = strings.TrimPrefix(note[s]+" and identity.bootstrapAdmin", " and ")
		}
		for _, name := range sortedKeys(keys) {
			add(Secret{Name: name, Kind: "generic", Keys: keys[name], Why: note[name]})
		}

		if v.Identity.SSO.Issuer != "" && v.Identity.SSO.ExistingSecret != "" {
			add(Secret{
				Name: v.Identity.SSO.ExistingSecret,
				Kind: "generic",
				Keys: []string{orDefault(v.Identity.SSO.ClientSecretKey, "clientSecret")},
				Why:  "identity.sso - the client secret, which is never a value in Git",
			})
		}
	}

	// CloudNativePG publishes the connection itself, so only a database this
	// deployment does not own needs one.
	external := v.Database.Cluster.Name != nil && *v.Database.Cluster.Name == ""
	if external && v.Database.ExistingSecret != "" {
		add(Secret{
			Name: v.Database.ExistingSecret,
			Kind: "generic",
			Keys: []string{"username", "password"},
			Why:  "database.existingSecret - this instance provisions no database",
		})
	}

	if byHand {
		inv, err := inventoryEntries(root)
		if err != nil {
			return nil, err
		}
		pull := map[string]bool{}
		for _, name := range v.ImagePullSecrets {
			pull[name] = true
		}
		for _, e := range inv {
			if pull[e.Name] {
				continue
			}
			add(Secret{
				Name: e.Name,
				Kind: kindFor(e.Keys),
				Keys: e.Keys,
				Why:  "config/secrets/secrets.yaml - " + firstLine(e.Description),
			})
		}
	}

	return out, nil
}

type inventoryEntry struct {
	Name        string   `json:"name"`
	Keys        []string `json:"keys"`
	Description string   `json:"description"`
}

func inventoryEntries(root string) ([]inventoryEntry, error) {
	rel := "config/secrets/secrets.yaml"
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) // #nosec G304 -- fixed path.
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	var doc struct {
		Secrets []inventoryEntry `json:"secrets"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	return doc.Secrets, nil
}

// instanceNamespace reads the namespace the instance's kustomization stamps on
// everything it applies.
func instanceNamespace(root, instance string) (string, error) {
	rel := path.Join(InstanceDir(instance), "kustomization.yaml")
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) // #nosec G304 -- instance passed checkSegment.
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}
	var k struct {
		Namespace string `json:"namespace"`
	}
	if err := yaml.Unmarshal(b, &k); err != nil {
		return "", fmt.Errorf("parse %s: %w", rel, err)
	}
	if k.Namespace == "" {
		return "", fmt.Errorf("%s sets no namespace", rel)
	}
	return k.Namespace, nil
}

func kindFor(keys []string) string {
	if len(keys) == 1 && keys[0] == ".dockerconfigjson" {
		return "docker-registry"
	}
	return "generic"
}

// createCommand is the command itself, with a placeholder per key rather than a
// value. A placeholder that looks like a value is a credential somebody pastes
// without reading, so every one of them names what belongs there.
func createCommand(s Secret) []string {
	head := fmt.Sprintf("kubectl -n %s create secret %s %s", s.Namespace, s.Kind, s.Name)
	if s.Kind == "docker-registry" {
		return []string{
			head,
			`  --docker-server="$REGISTRY_HOST"`,
			`  --docker-username="$REGISTRY_USERNAME"`,
			`  --docker-password="$REGISTRY_TOKEN"`,
		}
	}
	lines := []string{head}
	for _, k := range s.Keys {
		lines = append(lines, fmt.Sprintf(`  --from-literal=%s="$%s"`, k, envName(s.Name, k)))
	}
	return lines
}

// envName is the shell variable a key's value is read from, so the commands can
// be pasted after the values have been exported and nothing lands in history.
func envName(secret, key string) string {
	clean := func(s string) string {
		s = strings.TrimPrefix(s, ".")
		s = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return r
			default:
				return '_'
			}
		}, s)
		return strings.ToUpper(s)
	}
	return clean(secret) + "_" + clean(key)
}

// WriteSecretSetup prints the commands for one instance.
func WriteSecretSetup(w io.Writer, root, instance string) error {
	secrets, err := RequiredSecrets(root, instance)
	if err != nil {
		return err
	}
	if len(secrets) == 0 {
		fmt.Fprintf(w, "# %s needs no Secret created by hand.\n", instance)
		return nil
	}
	ns := secrets[0].Namespace

	fmt.Fprintf(w, "# %s - %d Secret(s) to create before Flux installs anything.\n", instance, len(secrets))
	fmt.Fprintf(w, "# Generated from deploy/flux/instances/%s/values/values.yaml.\n\n", instance)
	fmt.Fprintf(w, "kubectl create namespace %s\n", ns)

	for _, s := range secrets {
		fmt.Fprintf(w, "\n# %s\n", s.Why)
		for i, line := range s.Command {
			suffix := " \\"
			if i == len(s.Command)-1 {
				suffix = ""
			}
			fmt.Fprintf(w, "%s%s\n", line, suffix)
		}
	}
	return nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func firstLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i]
	}
	return strings.TrimSuffix(s, ".")
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
