package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func newPackagesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "packages",
		Aliases: []string{"package", "pkg"},
		Short:   "Inspect discovered packages",
		Long: "A package is one version of a vendor product: a specific tag\n" +
			"resolved to a specific manifest digest. Different tags are\n" +
			"independent versions that coexist — discovering v2.14.0 does\n" +
			"nothing to v2.13.0.",
	}
	cmd.AddCommand(
		newPackagesListCommand(),
		newPackagesDescribeCommand(),
		newPackagesDiscoverCommand(),
	)
	return cmd
}

func newPackagesListCommand() *cobra.Command {
	var (
		repository string
		tag        string
		state      string
		pageSize   int
		pageToken  string
		all        bool
	)

	cmd := &cobra.Command{
		Use:   "list <product>",
		Short: "List a product's discovered packages",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			opts0 := v1.ListPackagesOptions{
				Repository: repository,
				Tag:        tag,
				State:      strings.ToUpper(state),
				PageSize:   pageSize,
				PageToken:  pageToken,
			}

			resp, err := client.ListPackages(cmd.Context(), args[0], opts0)
			if err != nil {
				return err
			}

			// --all follows the cursor rather than making the user paste tokens
			// back. Bounded so a mistake cannot spin forever against a large
			// repository.
			if all {
				const maxPages = 100
				for page := 0; resp.NextPageToken != "" && page < maxPages; page++ {
					opts0.PageToken = resp.NextPageToken
					next, err := client.ListPackages(cmd.Context(), args[0], opts0)
					if err != nil {
						return err
					}
					resp.Packages = append(resp.Packages, next.Packages...)
					resp.NextPageToken = next.NextPageToken
				}
			}

			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderPackageList(w, resp)
			})
		},
	}

	cmd.Flags().StringVar(&repository, "repository", "",
		"show only packages from this repository path, e.g. suite/core")
	cmd.Flags().StringVar(&tag, "tag", "", "show only packages with this tag")
	cmd.Flags().StringVar(&state, "state", "", "filter by state, e.g. discovered or superseded")
	cmd.Flags().IntVar(&pageSize, "page-size", 0, "results per page")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "continue from a previous nextPageToken")
	cmd.Flags().BoolVar(&all, "all", false, "fetch every page")
	return cmd
}

func renderPackageList(w io.Writer, resp *v1.ListPackagesResponse) error {
	if len(resp.Packages) == 0 {
		fmt.Fprintln(w, "No packages discovered yet.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Discovery polls each source on its own interval. To scan now:")
		fmt.Fprintln(w, "  transferctl packages discover <product>")
		return nil
	}

	// The repository column appears only when the product actually spans more
	// than one. For the common single-repository product it would be a column
	// of identical values pushing everything else off the terminal.
	multiRepo := spansRepositories(resp.Packages)

	tw := newTabWriter(w)
	if multiRepo {
		fmt.Fprintln(tw, "REPOSITORY\tTAG\tDIGEST\tSTATE\tSIZE\tARTIFACTS\tBLOBS\tDISCOVERED")
	} else {
		fmt.Fprintln(tw, "TAG\tDIGEST\tSTATE\tSIZE\tARTIFACTS\tBLOBS\tDISCOVERED")
	}
	for _, p := range resp.Packages {
		if multiRepo {
			fmt.Fprintf(tw, "%s\t", dash(p.SourceRepository))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n",
			p.Tag,
			shortDigest(p.ManifestDigest),
			strings.ToLower(string(p.State)),
			humanBytes(p.TotalBytes),
			p.ArtifactCount,
			p.BlobCount,
			shortTime(p.DiscoveredAt),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// A superseded package is a vendor having re-pushed a released tag. Worth
	// calling out rather than leaving as an unexplained state column.
	superseded := 0
	for _, p := range resp.Packages {
		if p.State == v1.PackageSuperseded {
			superseded++
		}
	}
	if superseded > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d package(s) superseded: the vendor re-pushed the same tag with\n", superseded)
		fmt.Fprintln(w, "different content. The earlier rows are kept so the transfer history")
		fmt.Fprintln(w, "of what was actually shipped stays answerable.")
	}

	if resp.NextPageToken != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "More results. Continue with --page-token %s, or pass --all.\n", resp.NextPageToken)
	}
	return nil
}

// spansRepositories reports whether a listing covers more than one repository.
func spansRepositories(pkgs []v1.Package) bool {
	var first string
	for i, p := range pkgs {
		if i == 0 {
			first = p.SourceRepository
			continue
		}
		if p.SourceRepository != first {
			return true
		}
	}
	return false
}

func newPackagesDescribeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "describe <product> <tag-or-digest>",
		Short: "Show a package's artifact tree and transfer status",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			pkg, err := client.GetPackage(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			artifacts, err := client.ListArtifacts(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}

			combined := struct {
				Package   *v1.Package   `json:"package"`
				Artifacts []v1.Artifact `json:"artifacts"`
			}{Package: pkg, Artifacts: artifacts.Artifacts}

			return render(stdout(), opts.output, combined, func(w io.Writer) error {
				return renderPackageDetail(w, pkg, artifacts.Artifacts)
			})
		},
	}
}

func renderPackageDetail(w io.Writer, p *v1.Package, artifacts []v1.Artifact) error {
	fmt.Fprintf(w, "Package      %s\n", p.Tag)
	fmt.Fprintf(w, "Product      %s\n", p.Product)
	if p.SourceRepository != "" {
		fmt.Fprintf(w, "Repository   %s\n", p.SourceRepository)
	}
	fmt.Fprintf(w, "Digest       %s\n", p.ManifestDigest)
	fmt.Fprintf(w, "Media type   %s\n", p.MediaType)
	fmt.Fprintf(w, "State        %s\n", strings.ToLower(string(p.State)))
	fmt.Fprintf(w, "Size         %s across %d artifact(s) and %d blob(s)\n",
		humanBytes(p.TotalBytes), p.ArtifactCount, p.BlobCount)
	fmt.Fprintf(w, "Discovered   %s\n", p.DiscoveredAt)

	if p.SupersededBy != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "SUPERSEDED by package %s. The vendor re-pushed this tag with different\n", p.SupersededBy)
		fmt.Fprintln(w, "content. This row is kept so what was shipped from it stays answerable.")
	}

	if len(artifacts) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Artifact tree")
		renderArtifactTree(w, artifacts)
	}

	// The honest status line. A request sitting in `pending` after M2 is
	// correct behaviour, not a stall, and saying so here is cheaper than the
	// bug report that otherwise follows.
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Transfer status")
	fmt.Fprintln(w, "  Byte transfer is not implemented in this build. Discovery records")
	fmt.Fprintln(w, "  packages and auto-download rules create transfer requests, but the")
	fmt.Fprintln(w, "  queue and workers that execute them land in the next milestone, so")
	fmt.Fprintln(w, "  a request will stay pending. That is expected, not a failure.")

	return nil
}

// renderArtifactTree draws the manifest tree with indentation by depth.
func renderArtifactTree(w io.Writer, artifacts []v1.Artifact) {
	// Children are grouped under their parent so the printed order is the tree
	// order, not the row order.
	children := map[string][]v1.Artifact{}
	var roots []v1.Artifact
	for _, a := range artifacts {
		if a.ParentID == "" {
			roots = append(roots, a)
			continue
		}
		children[a.ParentID] = append(children[a.ParentID], a)
	}

	var draw func(a v1.Artifact, indent string)
	draw = func(a v1.Artifact, indent string) {
		label := shortDigest(a.Digest)
		if a.Platform != "" {
			label += "  " + a.Platform
		}
		fmt.Fprintf(w, "%s%s  %s  %s\n",
			indent, label, humanBytes(a.SizeBytes), shortMediaType(a.MediaType))
		for _, c := range children[a.ArtifactID] {
			draw(c, indent+"    ")
		}
	}
	for _, r := range roots {
		draw(r, "  ")
	}
}

func newPackagesDiscoverCommand() *cobra.Command {
	var source string

	cmd := &cobra.Command{
		Use:   "discover <product>",
		Short: "Scan a product's sources now, without waiting for the interval",
		Long: "Runs the same full scan the discovery loop runs, immediately.\n\n" +
			"Safe to repeat: a re-scan of unchanged content discovers nothing,\n" +
			"and concurrent triggers collapse into the running scan rather than\n" +
			"starting a second one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().DiscoverPackages(cmd.Context(), args[0], source)
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderDiscoverResult(w, args[0], resp)
			})
		},
	}

	cmd.Flags().StringVar(&source, "source", "", "scan only this source (default: every source)")

	// A full scan lists every tag of every repository, then resolves each one.
	// On a registry with a few hundred repositories that is minutes of honest
	// work, not a stall.
	contactsRegistries(cmd)
	return cmd
}

func renderDiscoverResult(w io.Writer, productName string, r *v1.DiscoverPackagesResponse) error {
	fmt.Fprintf(w, "Scanned %s in %s\n", productName, humanMillis(r.DurationMs))
	fmt.Fprintln(w)

	tw := newTabWriter(w)
	fmt.Fprintf(tw, "  Repositories scanned\t%d\n", r.Repositories)
	if r.RepositoriesFromCatalog > 0 {
		fmt.Fprintf(tw, "    found in the catalog\t%d\n", r.RepositoriesFromCatalog)
	}
	if r.RepositoriesFiltered > 0 {
		fmt.Fprintf(tw, "    rejected by filters\t%d\n", r.RepositoriesFiltered)
	}
	fmt.Fprintf(tw, "  Tags listed\t%d\n", r.TagsListed)
	fmt.Fprintf(tw, "  Tags after filters\t%d\n", r.TagsAdmitted)
	fmt.Fprintf(tw, "  New packages\t%d\n", r.PackagesDiscovered)
	fmt.Fprintf(tw, "  Superseded\t%d\n", r.Superseded)
	fmt.Fprintf(tw, "  Transfer requests created\t%d\n", r.RequestsCreated)
	if err := tw.Flush(); err != nil {
		return err
	}

	if r.PackagesDiscovered == 0 && len(r.TagErrors) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Nothing new. A scan that finds nothing is the normal steady state,")
		fmt.Fprintln(w, "not a failure.")
	}

	if r.RequestsCreated > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d auto-download rule match(es) created transfer requests. Byte\n", r.RequestsCreated)
		fmt.Fprintln(w, "transfer is not implemented in this build, so they will stay pending")
		fmt.Fprintln(w, "until the queue and workers land in the next milestone.")
	}

	if len(r.RepositoryErrors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d repository/repositories could not be read:\n", len(r.RepositoryErrors))
		for _, e := range r.RepositoryErrors {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}

	if len(r.TagErrors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d tag(s) could not be read. The rest of the scan completed:\n", len(r.TagErrors))
		for _, e := range r.TagErrors {
			fmt.Fprintf(w, "  %s\n", e)
		}
		// Exit non-zero so a script notices, while still printing what worked.
		return partialFailureError{msg: fmt.Sprintf("%d tag(s) failed during the scan", len(r.TagErrors))}
	}
	if len(r.RepositoryErrors) > 0 {
		return partialFailureError{
			msg: fmt.Sprintf("%d repository/repositories failed during the scan", len(r.RepositoryErrors))}
	}
	return nil
}

// ---------------------------------------------------------------------------
// formatting helpers
// ---------------------------------------------------------------------------

func shortDigest(d string) string {
	algo, hex, ok := strings.Cut(d, ":")
	if !ok || len(hex) < 12 {
		return d
	}
	return algo + ":" + hex[:12]
}

// humanBytes renders an AIP-141 string quantity.
//
// Binary units, because registries, image tooling and every other number an
// operator compares this against use them.
func humanBytes(v v1.Int64String) string {
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil {
		return string(v)
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func humanMillis(ms int64) string {
	if ms < 1000 {
		return strconv.FormatInt(ms, 10) + "ms"
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// shortTime trims an RFC 3339 timestamp to minutes for table display.
func shortTime(ts string) string {
	if len(ts) >= 16 {
		return ts[:16]
	}
	return ts
}

// shortMediaType drops the vendor prefix, which is identical on every row and
// pushes the informative part off the edge of a terminal.
func shortMediaType(mt string) string {
	mt = strings.TrimPrefix(mt, "application/vnd.")
	mt = strings.TrimSuffix(mt, "+json")
	return mt
}
