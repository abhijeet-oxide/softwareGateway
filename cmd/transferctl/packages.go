package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func newPackagesCommand() *cobra.Command {
	cmd := group(&cobra.Command{
		Use:     "packages",
		Aliases: []string{"package", "pkg"},
		Short:   "Inspect discovered packages",
		Long: "A package is one version of a vendor product: a specific tag\n" +
			"resolved to a specific manifest digest. Different tags are\n" +
			"independent versions that coexist - discovering v2.14.0 does\n" +
			"nothing to v2.13.0.",
	})
	cmd.AddCommand(
		newPackagesListCommand(),
		newPackagesDescribeCommand(),
		newPackagesInspectCommand(),
		newPackagesUnavailableCommand(),
		newPackagesDiscoverAliasCommand(),
	)
	return cmd
}

// newPackagesUnavailableCommand lists what the vendor will not serve.
//
// The counterpart of `packages list`, and the reason it is a separate command
// rather than a filter on it: these are the ABSENCE of packages. A scan meets
// them on every pass and can do nothing about them, so they belong neither in
// the catalogue nor in an error - they belong in a listing somebody consults
// when they are asking why a release they expected is not there.
func newPackagesUnavailableCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unavailable",
		Short: "List content a source would not serve",
		Long: "Content a scan asked for and was refused, most recently confirmed\n" +
			"first. A vendor registry serves a catalogue spanning every customer,\n" +
			"so being refused the products this account has not licensed is the\n" +
			"entitlement check working rather than anything to fix.\n\n" +
			"A row that stops being refreshed is one that came back: it is deleted\n" +
			"the first time a scan reads it successfully.",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ListUnavailable(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderUnavailable(w, resp)
			})
		},
	}

	takes(cmd, "unavailable", productArg())
	return cmd
}

func renderUnavailable(w io.Writer, r *v1.ListUnavailableResponse) error {
	if len(r.Packages) == 0 {
		fmt.Fprintln(w, "Nothing has been refused.")
		return nil
	}

	tw := newTabWriter(w)
	fmt.Fprintln(tw, "REPOSITORY\tVERSION\tREASON\tFIRST SEEN\tLAST SEEN")
	for _, u := range r.Packages {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			firstNonEmpty(u.DisplayRepository, u.Repository),
			firstNonEmpty(u.DisplayTag, u.Tag),
			strings.ReplaceAll(u.Reason, "_", " "),
			shortTime(u.FirstSeenAt),
			shortTime(u.LastSeenAt),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// The vendor's own sentence, once per distinct message. It names the end
	// user and the sales item, which is what somebody takes to their account
	// manager - and it is identical across dozens of rows.
	said := map[string]bool{}
	for _, u := range r.Packages {
		if u.Detail == "" || said[u.Detail] {
			continue
		}
		said[u.Detail] = true
		fmt.Fprintf(w, "\n%s\n", u.Detail)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "-"
}

func newPackagesListCommand() *cobra.Command {
	var (
		repository  string
		tag         string
		state       string
		accessories bool
		pageSize    int
		pageToken   string
		all         bool
		wide        bool
	)

	cmd := &cobra.Command{
		Short: "List a product's discovered packages",
		Long: "Where a source declares a `vendor`, the TAG and REPOSITORY columns\n" +
			"show that vendor's shortened spelling - `cfx-5000-k8s` rather than\n" +
			"`orbs/cfx-5000-k8s`, `23.8.1076` rather than `orb_23.8.1076`. The full\n" +
			"names are what is stored, transferred and returned by `-o json`; only\n" +
			"the table is shortened, and only for a source that says its vendor\n" +
			"does this.\n\n" +
			"--repository and --tag accept EITHER spelling, so a value copied off\n" +
			"this listing can be pasted straight back in.",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()
			opts0 := v1.ListPackagesOptions{
				Repository:         repository,
				Tag:                tag,
				State:              strings.ToUpper(state),
				IncludeAccessories: accessories,
				PageSize:           pageSize,
				PageToken:          pageToken,
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
				return renderPackageList(w, resp, wide)
			})
		},
	}

	cmd.Flags().StringVar(&repository, "repository", "",
		"show only packages from this repository, full or shortened, e.g. suite/core")
	cmd.Flags().StringVar(&tag, "tag", "",
		"show only packages with this tag, full or shortened, e.g. orb_23.8.1076 or 23.8.1076")
	cmd.Flags().StringVar(&state, "state", "", "filter by state, e.g. discovered or superseded")
	cmd.Flags().BoolVar(&accessories, "include-accessories", false,
		"also show signature and wrapper tags, which belong to a release rather than being one")
	cmd.Flags().IntVar(&pageSize, "page-size", 0, "results per page")
	cmd.Flags().StringVar(&pageToken, "page-token", "", "continue from a previous nextPageToken")
	cmd.Flags().BoolVar(&all, "all", false, "fetch every page")
	cmd.Flags().BoolVar(&wide, "wide", false,
		"add the bookkeeping columns: STATE, which is about the CATALOGUE - "+
			"whether the vendor has re-pushed this tag - and not about transfers")

	takes(cmd, "list", productArg())
	return cmd
}

// renderPackageList prints the catalogue.
//
// STATE is behind --wide because it answers a question almost nobody is asking
// here. It has exactly two values in practice - `discovered`, and `superseded`
// once the vendor re-pushes a tag - so it is a column of one repeated word, and
// a reader who sees a column called STATE on a page about packages reasonably
// reads it as "has this been transferred?", which it has never meant. Where a
// package has GOT to is a question about a package and a target, and `describe`
// answers it per target.
//
// It is still on every row of `-o json`: the machine-readable form has no width
// to spend and no reader to mislead.
func renderPackageList(w io.Writer, resp *v1.ListPackagesResponse, wide bool) error {
	if len(resp.Packages) == 0 {
		fmt.Fprintln(w, "No packages discovered yet.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Discovery polls each source on its own interval. To scan now:")
		fmt.Fprintln(w, "  transferctl discover <product>")
		return nil
	}

	// The repository column appears only when the product actually spans more
	// than one. For the common single-repository product it would be a column
	// of identical values pushing everything else off the terminal.
	multiRepo := spansRepositories(resp.Packages)

	tw := newTabWriter(w)
	// PUBLISHED before DISCOVERED, matching the sort order: the list is ordered
	// by when the vendor says a release was built, which is the order a person
	// thinks about releases in. DISCOVERED stays because the two genuinely
	// differ - a release published in March that we only saw in July is worth
	// being able to notice.
	header := "TAG\tDIGEST\tSIGNED\tSIZE\tARTIFACTS\tBLOBS\tPUBLISHED\tDISCOVERED"
	if wide {
		header = "TAG\tDIGEST\tSTATE\tSIGNED\tSIZE\tARTIFACTS\tBLOBS\tPUBLISHED\tDISCOVERED"
	}
	if multiRepo {
		header = "REPOSITORY\t" + header
	}
	fmt.Fprintln(tw, header)
	for _, p := range resp.Packages {
		if multiRepo {
			fmt.Fprintf(tw, "%s\t", dash(displayRepository(p)))
		}
		if wide {
			fmt.Fprintf(tw, "%s\t%s\t%s\t", displayTag(p),
				shortDigest(p.ManifestDigest), strings.ToLower(string(p.State)))
		} else {
			fmt.Fprintf(tw, "%s\t%s\t", displayTag(p), shortDigest(p.ManifestDigest))
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n",
			signedMark(p.SignatureStatus),
			humanBytesOpt(p.TotalBytes),
			p.ArtifactCount,
			optionalCount(p.BlobCount),
			optionalTime(p.PublishedAt),
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
	cmd := &cobra.Command{
		Short: "Show a package's contents and transfer status",
		Long: "<package> is a tag, a digest, or `repository:tag`. Where a source\n" +
			"declares a `vendor`, the shortened spellings a listing shows work\n" +
			"too - `cfx-5000-k8s:23.8.1076` and `orbs/cfx-5000-k8s:orb_23.8.1076`\n" +
			"are the same package.\n\n" +
			"A bare tag is AMBIGUOUS when a product spans several repositories -\n" +
			"a vendor's version tag appears in many of them - so a tag matching\n" +
			"more than one is refused with the list, rather than one being picked\n" +
			"for you. Scope it as `orbs/cfx-5000-k8s:orb_23.8.1076`.\n\n" +
			"This is a READ. It shows everything known about the package,\n" +
			"including the size and contents `packages inspect` gathered - so a\n" +
			"package that has been inspected describes fully, and one that has\n" +
			"not says so rather than guessing.",
		Aliases: []string{"show"},
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
				return renderPackageDetail(w, args[0], args[1], pkg, artifacts.Artifacts)
			})
		},
	}

	takes(cmd, "describe", productArg(), packageArg())
	return cmd
}

func renderPackageDetail(w io.Writer, product, ref string, p *v1.Package, artifacts []v1.Artifact) error {
	fmt.Fprintf(w, "Package      %s\n", p.Tag)
	if p.DisplayTag != "" && p.DisplayTag != p.Tag {
		// Shown once, here, and never substituted for the real tag: `describe`
		// is where someone comes to find out what a thing actually is, and the
		// stored name is the answer. The short form is listed as an alias so a
		// value copied out of a listing is recognisable as the same package.
		fmt.Fprintf(w, "             also accepted as %s\n", p.DisplayTag)
	}
	fmt.Fprintf(w, "Product      %s\n", p.Product)
	if p.SourceRepository != "" {
		fmt.Fprintf(w, "Repository   %s\n", p.SourceRepository)
		if p.DisplayRepository != "" && p.DisplayRepository != p.SourceRepository {
			fmt.Fprintf(w, "             also accepted as %s\n", p.DisplayRepository)
		}
	}
	fmt.Fprintf(w, "Digest       %s\n", p.ManifestDigest)
	fmt.Fprintf(w, "Media type   %s\n", p.MediaType)
	fmt.Fprintf(w, "State        %s\n", strings.ToLower(string(p.State)))
	fmt.Fprintf(w, "Signed       %s\n", describeSigned(p))
	fmt.Fprintf(w, "Size         %s across %d artifact(s) and %s blob(s)\n",
		humanBytesOpt(p.TotalBytes), p.ArtifactCount, optionalCount(p.BlobCount))

	// The two states a package can be in, said plainly. An inspected package
	// reports when it was measured, so a number nobody can date does not sit
	// beside one that can; an uninspected one says what is missing and how to
	// get it, rather than showing `n/a` and leaving the reader to guess.
	switch {
	case p.ExpandedAt != "":
		fmt.Fprintf(w, "Inspected    %s\n", p.ExpandedAt)
	case p.TotalBytes == nil:
		fmt.Fprintln(w, "             not measured - discovery records what this package's index")
		fmt.Fprintln(w, "             lists without fetching it.")
		fmt.Fprintf(w, "             transferctl packages inspect %s %s\n", product, ref)
	}

	fmt.Fprintf(w, "Discovered   %s\n", p.DiscoveredAt)
	if p.PublishedAt != "" {
		// Labelled as the vendor's claim, because that is what it is: an
		// annotation whoever published the artifact wrote. Discovered is ours.
		fmt.Fprintf(w, "Published    %s  (declared by the publisher)\n", p.PublishedAt)
	}

	if p.SupersededBy != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "SUPERSEDED by package %s. The vendor re-pushed this tag with different\n", p.SupersededBy)
		fmt.Fprintln(w, "content. This row is kept so what was shipped from it stays answerable.")
	}

	renderSignature(w, p)
	renderRelated(w, p)
	renderPackageTransfers(w, p)

	if len(artifacts) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Artifact tree")
		renderArtifactTree(w, artifacts)
		renderCacheNote(w, artifacts)
	}

	return nil
}

// renderPackageTransfers says what has been attempted with this package, and
// where.
//
// # Why this is a list and not a state
//
// "Has it been transferred?" has as many answers as there are targets. A column
// on the package could hold one of them, would be wrong the moment a second
// target was configured, and could not say WHICH target it meant. The per-pair
// truth already exists - one transfer row per package and destination - so this
// reads it rather than reducing it to something smaller than the question.
//
// # Why the failure reason is printed in full
//
// It is what makes a refusal actionable weeks later. A vendor declining one
// component of a release says so in its own words, naming the customer and the
// sales item, and the DIGEST in that message is what turns "an entitlement is
// missing" into "this component is the one". Neither survives being reduced to
// a flag, and the transfer that met it is where both are recorded.
func renderPackageTransfers(w io.Writer, p *v1.Package) {
	if len(p.Transfers) == 0 {
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Transfers")
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  TARGET\tSTATE\tWHEN\tID")
	for _, t := range p.Transfers {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			dash(t.Target), strings.ToLower(string(t.State)),
			shortTime(firstNonEmpty(t.CompletedAt, t.CreatedAt)), shortID(t.ID))
	}
	if err := tw.Flush(); err != nil {
		return
	}

	// Under the table rather than in it: a reason is a sentence, and a column
	// wide enough for one would leave the other three unreadable.
	for _, t := range p.Transfers {
		if t.FailureReason == "" {
			continue
		}
		fmt.Fprintf(w, "  %s failed: %s\n", shortID(t.ID), t.FailureReason)
	}
}

// renderCacheNote explains a tree whose manifest bodies have been reclaimed.
//
// Printed only when it applies, and worded so it does not read as a problem -
// because it is not one. Nothing about the package is unknown; the bodies are a
// cache in front of the source registry and the sweeper reclaimed some. Without
// the note, an operator comparing two `describe` outputs would see the same
// package apparently lose something between them.
func renderCacheNote(w io.Writer, artifacts []v1.Artifact) {
	dropped := 0
	for _, a := range artifacts {
		if a.Fetched && !a.Cached {
			dropped++
		}
	}
	if dropped == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %d of %d manifest bodies are no longer held locally. Their contents,\n",
		dropped, len(artifacts))
	fmt.Fprintln(w, "  sizes and blobs are recorded above and nothing is missing - the bodies")
	fmt.Fprintln(w, "  are a bounded cache, and a transfer re-reads them from the source.")
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
		if name := a.Annotations["org.opencontainers.image.ref.name"]; name != "" {
			// The one annotation worth promoting into the tree: it is the
			// difference between a listing of digests and a listing a person
			// can read.
			label = name + "  " + label
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
	fmt.Fprintf(w, "  transferctl discover status %s     # watch it\n", productName)
	fmt.Fprintf(w, "  transferctl packages list %s       # results, once it finishes\n", productName)
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

	// The vendor's nouns, from the SERVER's vendor plugin. A NEAR operator does
	// not have repositories and tags, they have orbs and orb versions, and
	// having to translate every line of a summary is the same tax as reading
	// `orbs/` on every row of a listing.
	words := scanWords(r.Vocabulary)

	// Two populations that look alike and mean opposite things: content the
	// vendor will not sell us, and content we could not read. The first is a
	// standing fact about the catalogue; the second is a fault. Only the second
	// makes the command exit non-zero.
	var notEntitled, faults []v1.ScanIssue
	for _, e := range r.TagErrors {
		if e.Class == classNotEntitled {
			notEntitled = append(notEntitled, e)
			continue
		}
		faults = append(faults, e)
	}

	tw := newTabWriter(w)
	fmt.Fprintf(tw, "  %s scanned\t%d\n", sentenceCase(words.Units), r.Repositories)
	if r.RepositoriesFromCatalog > 0 {
		fmt.Fprintf(tw, "    found in the catalog\t%d\n", r.RepositoriesFromCatalog)
	}
	if r.RepositoriesFiltered > 0 {
		fmt.Fprintf(tw, "    rejected by filters\t%d\n", r.RepositoriesFiltered)
	}
	fmt.Fprintf(tw, "  %s listed\t%d\n", sentenceCase(words.Versions), r.TagsListed)
	fmt.Fprintf(tw, "  %s after filters\t%d\n", sentenceCase(words.Versions), r.TagsAdmitted)
	fmt.Fprintf(tw, "  New packages\t%d\n", r.PackagesDiscovered)
	fmt.Fprintf(tw, "  Superseded\t%d\n", r.Superseded)
	if r.Regrouped > 0 {
		// Same argument as the line below: this happens exactly once, on the
		// first scan after a source gains a vendor, and it is what an operator
		// is waiting to see.
		fmt.Fprintf(tw, "  Packages regrouped\t%d\n", r.Regrouped)
	}
	if r.Renamed > 0 {
		// Shown only when it happened, because it happens exactly once - on the
		// first scan after a source's `vendor` is edited. It is the direct
		// answer to "did my config change take effect", which is otherwise
		// answerable only by going and looking at a listing.
		fmt.Fprintf(tw, "  Display names corrected\t%d\n", r.Renamed)
	}
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
			fmt.Fprintln(w, "  Check `repositories:` on the source, or - if it names none -")
			fmt.Fprintln(w, "  whether the registry's /v2/_catalog returns anything.")
			fmt.Fprintln(w, "  `transferctl products check` answers both.")
		}

	case r.PackagesDiscovered == 0 && len(faults) == 0:
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Nothing new. A scan that finds nothing is the normal steady state,")
		fmt.Fprintln(w, "not a failure.")
	}

	renderNotEntitled(w, notEntitled, words)

	if len(r.RepositoryErrors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d repository/repositories could not be read:\n", len(r.RepositoryErrors))
		for _, e := range r.RepositoryErrors {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}

	if len(faults) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s could not be read. The rest of the scan completed:\n",
			plural(len(faults), words.Version, words.Versions))
		for _, e := range faults {
			fmt.Fprintf(w, "  %s\n", e.Message)
		}
		// Exit non-zero so a script notices, while still printing what worked.
		return partialFailureError{
			msg: fmt.Sprintf("%d %s failed during the scan", len(faults), words.Versions)}
	}
	if len(r.RepositoryErrors) > 0 {
		return partialFailureError{
			msg: fmt.Sprintf("%d repository/repositories failed during the scan", len(r.RepositoryErrors))}
	}
	return nil
}

// classNotEntitled is the server's class for a source refusing content this
// customer has not licensed. Matched rather than re-derived from the message,
// because the classification is the server's to make.
const classNotEntitled = "not_entitled"

// renderNotEntitled reports content the vendor will not serve this account.
//
// # Why this is not an error
//
// A vendor registry serves a catalogue spanning every customer. Asking about a
// product this customer has not licensed gets a 403, and that is the
// entitlement check WORKING - not an outage, not a bad credential, and nothing
// anybody here can fix. On a real catalogue it is dozens of orbs, on every
// scan, forever.
//
// Reported as failures it made every scheduled scan exit non-zero with
// thirty-seven lines of URL and status code attached, which is precisely how a
// monitoring signal comes to be ignored. So the scan reports success, because
// it succeeded, and this block states the fact.
//
// # Why it is grouped
//
// Thirty-seven lines of `orbs/cfx-5000-k8s tag orb_24.7.1186: HTTP 403:
// forbidden` differ only in the two parts that carry no information about what
// went wrong. Grouped by orb, with the versions listed after it, the same
// thirty-seven lines become four - and the registry's own sentence, which names
// the customer and the product and is the thing somebody takes to their account
// manager, is printed once instead of thirty-seven times.
func renderNotEntitled(w io.Writer, issues []v1.ScanIssue, words v1.ScanVocabulary) {
	if len(issues) == 0 {
		return
	}

	byUnit := map[string][]string{}
	var order []string
	seen := map[string]bool{}
	for _, e := range issues {
		unit := issueUnit(e)
		if !seen[unit] {
			seen[unit] = true
			order = append(order, unit)
		}
		byUnit[unit] = append(byUnit[unit], issueVersion(e))
	}
	sort.Strings(order)

	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s could not be read: no entitlement for this account.\n",
		plural(len(issues), words.Unit, words.Units))
	fmt.Fprintln(w)

	tw := newTabWriter(w)
	for _, unit := range order {
		versions := byUnit[unit]
		sort.Strings(versions)
		fmt.Fprintf(tw, "  %s\t%s\n", unit, strings.Join(versions, ", "))
	}
	_ = tw.Flush()

	// The vendor's own words, once per distinct message. Ours is a status code;
	// theirs names the end user and the sales item.
	said := map[string]bool{}
	for _, e := range issues {
		detail := entitlementSentence(e.Message)
		if detail == "" || said[detail] {
			continue
		}
		said[detail] = true
		fmt.Fprintf(w, "\n  %s\n", detail)
	}
}

// issueUnit is the vendor's name for the repository an issue is in.
func issueUnit(e v1.ScanIssue) string {
	if e.DisplayRepository != "" {
		return e.DisplayRepository
	}
	if e.Repository != "" {
		return e.Repository
	}
	return "(unknown)"
}

// issueVersion is the vendor's name for the tag an issue is on.
func issueVersion(e v1.ScanIssue) string {
	if e.DisplayTag != "" {
		return e.DisplayTag
	}
	return e.Tag
}

// entitlementSentence pulls the registry's own message out of a rendered
// failure, dropping the path and status code we wrapped it in.
//
// Anything without a recognisable message yields "", and the block simply shows
// no sentence - the grouping above it is the useful half either way.
func entitlementSentence(message string) string {
	i := strings.LastIndex(message, "HTTP 403: ")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(message[i+len("HTTP 403: "):])
	// The sentinel we append - "forbidden" - is our word, not theirs.
	rest = strings.TrimSuffix(rest, ": forbidden")
	if rest == "" || rest == "forbidden" {
		return ""
	}
	return rest
}

// scanWords fills in the standard OCI nouns wherever the source's vendor
// supplies none, which is every conformant registry.
func scanWords(v *v1.ScanVocabulary) v1.ScanVocabulary {
	out := v1.ScanVocabulary{
		Unit: "repository", Units: "repositories", Version: "tag", Versions: "tags",
	}
	if v == nil {
		return out
	}
	if v.Unit != "" {
		out.Unit = v.Unit
	}
	if v.Units != "" {
		out.Units = v.Units
	}
	if v.Version != "" {
		out.Version = v.Version
	}
	if v.Versions != "" {
		out.Versions = v.Versions
	}
	return out
}

// sentenceCase capitalises the first letter of a label.
func sentenceCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
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
		return notAvailable
	}
	return humanBytes(*v)
}

func humanBytes(v v1.Int64String) string {
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil {
		return string(v)
	}
	return humanBytesOf(n)
}

// humanBytesOf renders a count this package has already computed, rather than
// one that arrived over the wire as a string.
func humanBytesOf(n int64) string {
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

func newDiscoverStatusCommand() *cobra.Command {
	var watch bool

	cmd := &cobra.Command{
		Short: "Show what discovery is doing right now",
		Long: "Reports the live state of every source: whether a scan is running,\n" +
			"which phase it is in, which repository it is on, and how the last\n" +
			"completed scan finished.\n\n" +
			"This is the answer to \"is it stuck or just slow?\", which a blocking\n" +
			"scan cannot give you while it is blocked.\n\n" +
			"With no argument, reports every product being polled.",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			product := ""
			if len(args) == 1 {
				product = args[0]
			} else {
				// No product named: report them all. Resolved here rather than
				// server-side so the output is one block per product, which is
				// what an operator scanning for the stuck one wants.
				return statusForEveryProduct(cmd.Context(), client, watch)
			}

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
				if !anyScanningIn(st) {
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

	takes(cmd, "status", optionalProductArg("reports every product being polled"))
	return cmd
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
				// CHECKED, not "resolved". The checked count moves as each HEAD
				// returns; the resolved one was reported in a single step when
				// the whole phase finished, so this line sat at "0 of 3180"
				// through the longest part of every scan.
				fmt.Fprintf(tw, "  Versions checked\t%d of %d", s.TagsChecked, s.TagsTotal)
				if s.TagsInFlight > 0 {
					fmt.Fprintf(tw, ", %d in flight", s.TagsInFlight)
				}
				fmt.Fprintln(tw)
			}
			if s.TagsToFetch > 0 {
				fmt.Fprintf(tw, "  New releases read\t%d of %d\n", s.TagsFetched, s.TagsToFetch)
			}
			if s.CurrentTag != "" {
				fmt.Fprintf(tw, "  Current version\t%s\n", s.CurrentTag)
			}
			if s.Packages > 0 {
				fmt.Fprintf(tw, "  Releases recorded so far\t%d\n", s.Packages)
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
// phase is waiting on - which is the part an operator actually needs.
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

// notAvailable is what a column shows when the value genuinely is not known.
//
// One token everywhere, and deliberately not "0" or "-": those read as
// measurements. A package whose root is an index has no measured size until
// something walks its tree, and the table has to say that rather than imply an
// empty package.
const notAvailable = "n/a"

// optionalCount renders a count that may not have been measured.
func optionalCount(n *int) string {
	if n == nil {
		return notAvailable
	}
	return strconv.Itoa(*n)
}

// optionalTime renders a timestamp the publisher may never have set.
func optionalTime(v string) string {
	if strings.TrimSpace(v) == "" {
		return notAvailable
	}
	return shortTime(v)
}

// newPackagesDiscoverAliasCommand keeps `packages discover` working.
//
// Hidden rather than removed: it is in scripts and in muscle memory, and
// breaking those to tidy a help screen is a bad trade. `transferctl discover`
// is the documented spelling.
func newPackagesDiscoverAliasCommand() *cobra.Command {
	cmd := newDiscoverCommand()
	cmd.Use = "discover [product]"
	cmd.Hidden = true
	cmd.Short = "Deprecated: use `transferctl discover`"
	return cmd
}

// newPackagesInspectCommand builds out what discovery deliberately left out.
//
// It was briefly folded into `describe --expand`, on the argument that a user
// wants to see a package rather than to inspect and then describe. Two things
// were wrong with that.
//
// A flag hid the cost. `describe` is a database read that answers instantly;
// `describe --expand` opens dozens of connections to a vendor's registry and
// can take minutes. Those are different operations and a boolean is not enough
// warning.
//
// And it was the wrong place for the RESULT. Inspecting is not a rendering
// option - it writes artifacts, blobs and a measured size, and everything that
// reads a package afterwards, `describe` and a transfer alike, sees them. So
// inspect is a verb of its own again, and `describe` simply shows what it
// gathered.
func newPackagesInspectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Pull a package's full contents and measure it",
		Long: "Discovery is deliberately light: it fetches the tag's own manifest and\n" +
			"records the artifacts that manifest lists, WITHOUT fetching them. That\n" +
			"answers \"what is new\" in two requests per tag rather than one per\n" +
			"artifact, and it means a package's transfer size is not yet known.\n\n" +
			"This builds out the rest: it fetches the listed artifacts, records\n" +
			"their blobs, and measures the bytes a transfer would actually move.\n" +
			"What it records is kept, so `packages describe` shows it from then on\n" +
			"and a transfer of the same package does not repeat the walk.\n\n" +
			"Safe and cheap to repeat. The tree under a digest cannot change, so a\n" +
			"second run fetches nothing and says so.\n\n" +
			"You never HAVE to run this: a transfer performs the same walk if\n" +
			"nobody has. It is for deciding whether you want one.",
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
	takes(cmd, "inspect", productArg(), packageArg())
	return cmd
}

func renderInspect(w io.Writer, product, ref string, r *v1.InspectPackageResponse) error {
	if r.AlreadyExpanded {
		fmt.Fprintf(w, "%s %s was already inspected; nothing was fetched.\n", product, ref)
	} else {
		fmt.Fprintf(w, "Inspected %s %s - fetched %d manifest(s).\n", product, ref, r.Fetched)
	}
	fmt.Fprintln(w)

	tw := newTabWriter(w)
	fmt.Fprintf(tw, "  Artifacts\t%d\n", r.Artifacts)
	fmt.Fprintf(tw, "  Blobs\t%d\n", r.Blobs)
	fmt.Fprintf(tw, "  Transfer size\t%s\n", humanBytes(r.TotalBytes))
	// Cached is reported next to the totals rather than buried, because it is
	// the one number here that can go DOWN without anything having changed
	// about the package: the manifest bodies are a bounded cache and the
	// sweeper reclaims the least recently used. Everything above it is
	// permanent.
	fmt.Fprintf(tw, "  Manifest bodies held\t%d of %d  (%s)\n",
		r.CachedManifests, r.Artifacts, humanBytes(r.CachedBytes))
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  transferctl packages describe %s %s   # the full artifact tree\n", product, ref)
	return nil
}

// statusForEveryProduct renders discovery status across the fleet.
//
// One block per product rather than a merged table: an operator running this
// is looking for the one that is stuck, and a flat list of sources from
// different products makes that harder, not easier.
func statusForEveryProduct(ctx context.Context, client *v1.Client, watch bool) error {
	products, err := client.ListProducts(ctx)
	if err != nil {
		return err
	}
	if len(products.Products) == 0 {
		fmt.Fprintln(stdout(), "No products are configured.")
		return nil
	}

	for {
		anyScanning := false
		for i, p := range products.Products {
			st, err := client.DiscoveryStatus(ctx, p.ProductID)
			if err != nil {
				return err
			}
			if i > 0 {
				fmt.Fprintln(stdout())
			}
			if err := renderDiscoveryStatus(stdout(), st); err != nil {
				return err
			}
			if anyScanningIn(st) {
				anyScanning = true
			}
		}
		if !watch || !anyScanning {
			return nil
		}

		fmt.Fprintln(stdout())
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func anyScanningIn(st *v1.DiscoveryStatusResponse) bool {
	for _, s := range st.Sources {
		if s.Scanning {
			return true
		}
	}
	return false
}

// renderSignature says what is known about a package's signature, and - the
// part that matters - what is NOT.
//
// Discovery answers one question: is a signature there? It finds the artifact
// the vendor published, records its media type and its digest, and stops.
// Nothing in this build CHECKS one: no chain is built, no trust root is
// consulted, no digest is verified against a key. That is a separate milestone.
//
// It is spelled out rather than implied because "SIGNED" in a listing is
// exactly the kind of word somebody makes a release decision on. A tool that
// prints it without saying what it did and did not do is worse than one that
// says nothing.
func renderSignature(w io.Writer, p *v1.Package) {
	if p.SignatureStatus != v1.SignatureSigned {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Signature")
	fmt.Fprintln(w, "  A signature artifact was FOUND and recorded. It has NOT been verified:")
	fmt.Fprintln(w, "  nothing in this build checks a signature against a trust root, so this")
	fmt.Fprintln(w, "  says the vendor published one, not that it is valid or that it is theirs.")

	// The MATERIAL, when it has been resolved. This is what a verifier reads,
	// and printing it is how someone checks a signature by hand today - pull the
	// blob by digest and run it through `openssl cms`.
	for _, r := range p.Related {
		if !strings.EqualFold(r.Role, "signature") {
			continue
		}
		fmt.Fprintln(w)
		switch {
		case r.BlobDigest != "":
			fmt.Fprintln(w, "  Signature material")
			tw := newTabWriter(w)
			fmt.Fprintf(tw, "    manifest\t%s\n", shortDigest(r.Digest))
			fmt.Fprintf(tw, "    blob\t%s\n", r.BlobDigest)
			fmt.Fprintf(tw, "    format\t%s\n", dash(r.BlobMediaType))
			fmt.Fprintf(tw, "    size\t%s\n", humanBytes(r.BlobSize))
			if r.ResolvedAt != "" {
				fmt.Fprintf(tw, "    resolved\t%s\n", r.ResolvedAt)
			}
			_ = tw.Flush()
		default:
			fmt.Fprintln(w, "  The signature's contents have not been read yet - only that it")
			fmt.Fprintln(w, "  exists. `packages inspect` fetches it and records the blob a")
			fmt.Fprintln(w, "  verifier would check.")
		}
		break
	}

	if p.TransferRootTag != "" {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  A transfer walks %s, which reaches both this package and\n", p.TransferRootTag)
		fmt.Fprintln(w, "  its signature, so the two travel together and the destination can be")
		fmt.Fprintln(w, "  verified once verification exists. Moving the payload alone would")
		fmt.Fprintln(w, "  foreclose that permanently.")
	}
}

// describeSigned explains the signature status in a sentence rather than a
// word, because `describe` is where someone goes when the one-word answer in
// the listing was not enough.
func describeSigned(p *v1.Package) string {
	switch p.SignatureStatus {
	case v1.SignatureSigned:
		out := "yes"
		for _, r := range p.Related {
			if r.Role == "SIGNATURE" {
				out += "  (" + shortMediaType(r.MediaType) + ")"
				if r.Tag != "" {
					out += ", at " + r.Tag
				}
				break
			}
		}
		// Verification is a separate question from presence, and conflating
		// them is exactly the mistake this line exists to prevent.
		return out + "  - present, NOT verified"
	case v1.SignatureUnsigned:
		return "no  - this source was checked and the publisher signed nothing"
	default:
		return notAvailable +
			"  - not checked; set `signatures.layout` on the source to look"
	}
}

// renderRelated lists the artifacts attached to a package.
func renderRelated(w io.Writer, p *v1.Package) {
	if len(p.Related) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Related artifacts")
	tw := newTabWriter(w)
	for _, r := range p.Related {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			strings.ToLower(r.Role), dash(r.Tag), shortDigest(r.Digest), shortMediaType(r.MediaType))
	}
	_ = tw.Flush()
}
