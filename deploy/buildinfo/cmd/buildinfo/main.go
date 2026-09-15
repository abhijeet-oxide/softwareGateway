// Command buildinfo publishes one release to Artifactory's Builds view, and
// promotes it when asked.
//
// Run by .github/workflows/cd.yml after the images and the chart are published.
// Every credential arrives in the environment, never as a flag: an argument is
// visible in `ps` and in the runner's own command echo.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/deploy/buildinfo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "buildinfo: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		name         = flag.String("name", "software-gateway", "Build Info name, stable across releases")
		number       = flag.String("number", "", "Build Info number; the chart version")
		images       = flag.String("images", "", "comma-separated component names, all at -image-version")
		imageVersion = flag.String("image-version", "", "the tag every image carries")
		chartName    = flag.String("chart", "software-gateway", "the chart's repository path")
		chartVersion = flag.String("chart-version", "", "the chart's version")
		registry     = flag.String("registry", "", "registry host")
		repository   = flag.String("repository", "", "repository path under the registry")
		promoteTo    = flag.String("promote-to", "", "promote the build to this repository once published")
		dryRun       = flag.Bool("dry-run", false, "print the document and publish nothing")
	)
	flag.Parse()

	// Credentials and context from the environment.
	var (
		base      = os.Getenv("BUILD_INFO_URL")
		user      = os.Getenv("REGISTRY_USERNAME")
		token     = os.Getenv("REGISTRY_TOKEN")
		revision  = os.Getenv("GITHUB_SHA")
		principal = os.Getenv("GITHUB_ACTOR")
	)
	repoURL := ""
	if server, repo := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"); server != "" && repo != "" {
		repoURL = server + "/" + repo
	}
	buildURL := ""
	if repoURL != "" && os.Getenv("GITHUB_RUN_ID") != "" {
		buildURL = repoURL + "/actions/runs/" + os.Getenv("GITHUB_RUN_ID")
	}

	for flagName, v := range map[string]string{
		"-number": *number, "-image-version": *imageVersion,
		"-chart-version": *chartVersion, "-registry": *registry, "-repository": *repository,
	} {
		if v == "" {
			return fmt.Errorf("%s is required", flagName)
		}
	}
	if *images == "" {
		return fmt.Errorf("-images is required: a release with no images is not this product")
	}
	if !*dryRun && (base == "" || token == "") {
		return fmt.Errorf("BUILD_INFO_URL and REGISTRY_TOKEN are required to publish; " +
			"pass -dry-run to see the document without one")
	}

	rel := buildinfo.Release{
		Name: *name, Number: *number, Started: time.Now().UTC(),
		Registry: *registry, Repository: *repository,
		Revision: revision, URL: repoURL, BuildURL: buildURL, Principal: principal,
	}
	comps := buildinfo.Components(split(*images), *imageVersion, *chartName, *chartVersion)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	reg := &buildinfo.Registry{Username: user, Token: token}
	bi, err := buildinfo.Collect(ctx, reg, rel, comps)
	if err != nil {
		return err
	}

	fmt.Printf("%s/%s describes %d components:\n", bi.Name, bi.Number, len(bi.Modules))
	for _, m := range bi.Modules {
		fmt.Printf("  %-12s %s %s\n", m.Type, m.ID, m.Properties["docker.image.id"])
	}

	if *dryRun {
		out, err := json.MarshalIndent(bi, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}

	client := &buildinfo.Client{BaseURL: base, Username: user, Token: token}
	if err := client.Publish(ctx, bi); err != nil {
		return err
	}
	fmt.Printf("published %s/%s\n", bi.Name, bi.Number)

	if *promoteTo != "" {
		p := buildinfo.Promotion{
			Status:     "Released",
			Comment:    "promoted by CD run " + os.Getenv("GITHUB_RUN_ID"),
			CIUser:     principal,
			SourceRepo: *repository,
			TargetRepo: *promoteTo,
			Copy:       true,
			Artifacts:  true,
			FailFast:   true,
		}
		if err := client.Promote(ctx, bi.Name, bi.Number, p); err != nil {
			return err
		}
		fmt.Printf("promoted %s/%s to %s\n", bi.Name, bi.Number, *promoteTo)
	}
	return nil
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
