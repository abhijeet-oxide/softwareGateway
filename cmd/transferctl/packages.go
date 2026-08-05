package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

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
		newPackagesDiscoveryStatusCommand(),
		newPackagesInspectCommand(),
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			p.Tag,
			shortDigest(p.ManifestDigest),
			strings.ToLower(string(p.State)),
			humanBytesOpt(p.TotalBytes),
			p.ArtifactCount,
			optionalCount(p.BlobCount),
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
	fmt.Fprintf(w, "Size         %s across %d artifact(s) and %s blob(s)\n",
		humanBytesOpt(p.TotalBytes), p.ArtifactCount, optionalCount(p.BlobCount))
	if p.TotalBytes == nil {
		fmt.Fprintln(w, "             (this package's root is an index; discovery records what it")
		fmt.Fprintln(w, "              lists without fetching it, so the layer bytes are not known")
		fmt.Fprintln(w, "              until a transfer walks the tree)")
	}
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
		suffix := ""
		if !a.Fetched {
			// The distinction is worth showing: a fetched manifest was verified
			// against its digest, a listed one has the vendor's word for it.
			suffix = "  (listed, not fetched)"
		}
		fmt.Fprintf(w, "%s%s  %s  %s%s\n",
			indent, label, humanBytes(a.SizeBytes), shortMediaType(a.MediaType), suffix)
		for _, c := range children[a.ArtifactID] {
			draw(c, indent+"    ")
		}
	}
	for _, r := range roots {
		draw(r, "  ")
	}
}

func newPackagesDiscoverCommand() *cobra.Command {
	var (
		source string
		wait   bool
	)

	cmd := &cobra.Command{
		Use:   "discover <product>",
		Short: "Scan a product's sources now, without waiting for the interval",
		Long: "Runs the same full scan the discovery loop runs, immediately.\n\n" +
			"Safe to repeat: a re-scan of unchanged content discovers nothing,\n" +
			"and concurrent triggers collapse into the running scan rather than\n" +
			"starting a second one.\n\n" +
			"A scan contacts the registry, so it can take minutes against a slow\n" +
			"one. Live progress is printed to stderr while it runs. If you stop\n" +
			"waiting, the scan continues on the Coordinator — `transferctl\n" +
			"packages discovery-status <product>` shows where it got to.\n\n" +
			"Use --wait=false to start the scan and return immediately.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			product := args[0]
			client := newClient()

			if !wait {
				resp, err := client.StartDiscovery(cmd.Context(), product, source)
				if err != nil {
					return err
				}
				return render(stdout(), opts.output, resp, func(w io.Writer) error {
					return renderDiscoverStarted(w, product, resp)
				})
			}

			// The progress watcher runs alongside the blocking request on its
			// own connection. Stopped as soon as the scan returns, so the
			// summary is never printed over a half-drawn progress line.
			watchCtx, stopWatch := context.WithCancel(cmd.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				watchDiscovery(watchCtx, client, product, stderr())
			}()

			resp, err := client.DiscoverPackages(cmd.Context(), product, source)

			stopWatch()
			<-done

			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderDiscoverResult(w, product, resp)
			})
		},
	}

	cmd.Flags().StringVar(&source, "source", "", "scan only this source (default: every source)")
	cmd.Flags().BoolVar(&wait, "wait", true,
		"wait for the scan to finish; --wait=false starts it and returns immediately")

	// A full scan lists every tag of every repository, then resolves each one.
	// On a registry with a few hundred repositories that is minutes of honest
	// work, not a stall.
	contactsRegistries(cmd)
	return cmd
}

// renderDiscoverStarted reports a scan launched with --wait=false.
func renderDiscoverStarted(w io.Writer, productName string, r *v1.DiscoverPackagesResponse) error {
	st := r.Started
	if st == nil {
		// The server answered a no-wait request with results, which means it
		// predates the flag. Say so rather than printing nothing.
		fmt.Fprintln(w, "The Coordinator ran the scan synchronously; it does not support --wait=false.")
		return renderDiscoverResult(w, productName, r)
	}

	switch {
	case st.Sources > 0 && st.AlreadyRunning > 0:
		fmt.Fprintf(w, "Started %d scan(s) of %s; %d source(s) were already scanning.\n",
			st.Sources, productName, st.AlreadyRunning)
	case st.Sources > 0:
		fmt.Fprintf(w, "Started %d scan(s) of %s.\n", st.Sources, productName)
	default:
		// Nothing new was started, and saying "started" would be false.
		fmt.Fprintf(w, "%s was already being scanned; nothing new was started.\n", productName)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  transferctl packages discovery-status %s     # watch it\n", productName)
	fmt.Fprintf(w, "  transferctl packages list %s                 # results, once it finishes\n", productName)
	return nil
}

func renderDiscoverResult(w io.Writer, productName string, r *v1.DiscoverPackagesResponse) error {
	if r.Collapsed {
		// Distinguished from a scan this command started, because the numbers
		// look identical and the meaning is not. Without this line a request
		// that joined a scan already under way is indistinguishable from one
		// that ran its own.
		fmt.Fprintf(w, "Joined a scan of %s already in progress (%s)\n",
			productName, humanMillis(r.DurationMs))
	} else {
		fmt.Fprintf(w, "Scanned %s in %s\n", productName, humanMillis(r.DurationMs))
	}
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

	switch {
	case r.Repositories == 0 && len(r.RepositoryErrors) == 0:
		// NOT the steady state, and saying so was actively misleading: zero
		// repositories scanned means nothing was looked at. Either every
		// candidate was rejected by repositoryFilters, or the source names no
		// repositories and the registry's catalog returned none.
		fmt.Fprintln(w)
		fmt.Fprintln(w, "No repositories were scanned, so nothing was looked at. This is not")
		fmt.Fprintln(w, "the same as finding nothing.")
		if r.RepositoriesFiltered > 0 {
			fmt.Fprintf(w, "\n  %d candidate(s) were rejected by discovery.repositoryFilters.\n",
				r.RepositoriesFiltered)
		} else {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "  Check `repositories:` on the source, or — if it names none —")
			fmt.Fprintln(w, "  whether the registry's /v2/_catalog returns anything.")
			fmt.Fprintln(w, "  `transferctl products check` answers both.")
		}

	case r.PackagesDiscovered == 0 && len(r.TagErrors) == 0:
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
// humanBytesOpt renders a byte count that may not have been measured.
//
// Nil is "not measured", not zero. A package whose root is an index has its
// layer bytes recorded only when something walks the tree, and printing "0 B"
// for one would be a claim nobody would think to question.
func humanBytesOpt(v *v1.Int64String) string {
	if v == nil {
		return "not measured"
	}
	return humanBytes(*v)
}

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

func newPackagesDiscoveryStatusCommand() *cobra.Command {
	var watch bool

	cmd := &cobra.Command{
		Use:     "discovery-status <product>",
		Aliases: []string{"status"},
		Short:   "Show what discovery is doing right now",
		Long: "Reports the live state of every source: whether a scan is running,\n" +
			"which phase it is in, which repository it is on, and how the last\n" +
			"completed scan finished.\n\n" +
			"This is the answer to \"is it stuck or just slow?\", which a blocking\n" +
			"`packages discover` cannot give you while it is blocked.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			product := args[0]
			client := newClient()

			if !watch {
				st, err := client.DiscoveryStatus(cmd.Context(), product)
				if err != nil {
					return err
				}
				return render(stdout(), opts.output, st, func(w io.Writer) error {
					return renderDiscoveryStatus(w, st)
				})
			}

			// --watch prints a fresh block each interval rather than redrawing
			// in place: a scrolling history is what you want when you are
			// waiting to see whether a counter moves.
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				st, err := client.DiscoveryStatus(cmd.Context(), product)
				if err != nil {
					return err
				}
				if err := renderDiscoveryStatus(stdout(), st); err != nil {
					return err
				}
				if !anyScanning(st) {
					return nil
				}
				fmt.Fprintln(stdout())
				select {
				case <-ticker.C:
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				}
			}
		},
	}

	cmd.Flags().BoolVar(&watch, "watch", false, "keep printing until no scan is running")
	return cmd
}

func anyScanning(st *v1.DiscoveryStatusResponse) bool {
	for _, s := range st.Sources {
		if s.Scanning {
			return true
		}
	}
	return false
}

func renderDiscoveryStatus(w io.Writer, st *v1.DiscoveryStatusResponse) error {
	if !st.Running {
		fmt.Fprintln(w, "Discovery is not running on this replica.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "It runs on the leader only. Either this replica is a follower, or every")
		fmt.Fprintln(w, "source is disabled. `transferctl health` shows which.")
		return nil
	}
	if len(st.Sources) == 0 {
		fmt.Fprintln(w, "No sources are being polled for this product.")
		return nil
	}

	for i, s := range st.Sources {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s / %s\n", s.Product, s.Source)

		tw := newTabWriter(w)
		if s.Scanning {
			fmt.Fprintf(tw, "  Status\tSCANNING (%s)\n", humanMillis(s.ElapsedMs))
			fmt.Fprintf(tw, "  Phase\t%s\n", humanPhase(s.Phase))
			if s.RepositoriesTotal > 0 {
				fmt.Fprintf(tw, "  Repositories\t%d of %d done, %d in flight\n",
					s.RepositoriesDone, s.RepositoriesTotal, s.RepositoriesInFlight)
			}
			if s.CurrentRepository != "" {
				fmt.Fprintf(tw, "  Current repository\t%s\n", s.CurrentRepository)
			}
			if s.TagsTotal > 0 {
				fmt.Fprintf(tw, "  Tags resolved\t%d of %d\n", s.TagsResolved, s.TagsTotal)
			}
			if s.Artifacts > 0 {
				fmt.Fprintf(tw, "  Manifests fetched\t%d\n", s.Artifacts)
			}
			if s.NewPackages > 0 {
				fmt.Fprintf(tw, "  New packages so far\t%d\n", s.NewPackages)
			}
			if s.Errors > 0 {
				fmt.Fprintf(tw, "  Errors so far\t%d\n", s.Errors)
			}
		} else {
			fmt.Fprintf(tw, "  Status\tidle\n")
			if s.IntervalSeconds > 0 {
				fmt.Fprintf(tw, "  Interval\t%s\n", (time.Duration(s.IntervalSeconds) * time.Second).String())
			}
		}

		if s.LastRunAt != "" {
			fmt.Fprintf(tw, "  Last scan\t%s (%s)\n", s.LastRunAt, humanMillis(s.LastDurationMs))
			fmt.Fprintf(tw, "    repositories\t%d\n", s.LastRepositories)
			fmt.Fprintf(tw, "    tags listed\t%d\n", s.LastTagsListed)
			fmt.Fprintf(tw, "    new packages\t%d\n", s.LastNewPackages)
		} else {
			fmt.Fprintf(tw, "  Last scan\tnone has completed yet\n")
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		if s.LastError != "" {
			fmt.Fprintln(w)
			fmt.Fprintf(w, "  Last scan FAILED: %s\n", s.LastError)
		}
	}
	return nil
}

// humanPhase turns the wire enum into something readable, and says what the
// phase is waiting on — which is the part an operator actually needs.
func humanPhase(p string) string {
	switch p {
	case "ENUMERATING_REPOSITORIES":
		return "listing the registry's repositories (/v2/_catalog)"
	case "LISTING_TAGS":
		return "listing tags (/v2/<repo>/tags/list)"
	case "RESOLVING_TAGS":
		return "fetching manifests"
	case "", "IDLE":
		return "idle"
	default:
		return p
	}
}

// optionalCount renders a count that may not have been measured.
func optionalCount(n *int) string {
	if n == nil {
		return "?"
	}
	return strconv.Itoa(*n)
}

func newPackagesInspectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect <product> <tag-or-digest>",
		Short: "Expand a package's contents and measure its transfer size",
		Long: "Discovery is deliberately light: it fetches the tag's own manifest and\n" +
			"records the artifacts that manifest lists, without fetching them. That\n" +
			"answers \"what is new\" in two requests per tag, and it means a package's\n" +
			"transfer size is not yet known.\n\n" +
			"This walks the rest: it fetches the listed artifacts, records their\n" +
			"blobs, and measures the bytes a transfer would move.\n\n" +
			"Safe to repeat. The tree under a digest cannot change, so a second run\n" +
			"fetches nothing and says so. A transfer performs the same walk, so you\n" +
			"do not have to run this first — it is for deciding whether you want to.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().InspectPackage(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderInspect(w, args[0], args[1], resp)
			})
		},
	}

	// It reads from the vendor's registry, so it belongs with the slow commands.
	contactsRegistries(cmd)
	return cmd
}

func renderInspect(w io.Writer, product, ref string, r *v1.InspectPackageResponse) error {
	if r.AlreadyExpanded {
		fmt.Fprintf(w, "%s %s was already expanded; nothing was fetched.\n", product, ref)
	} else {
		fmt.Fprintf(w, "Expanded %s %s — fetched %d manifest(s).\n", product, ref, r.Fetched)
	}
	fmt.Fprintln(w)

	tw := newTabWriter(w)
	fmt.Fprintf(tw, "  Artifacts\t%d\n", r.Artifacts)
	fmt.Fprintf(tw, "  Blobs\t%d\n", r.Blobs)
	fmt.Fprintf(tw, "  Transfer size\t%s\n", humanBytes(r.TotalBytes))
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  transferctl packages describe %s %s   # the full artifact tree\n", product, ref)
	return nil
}
