package generic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// defaultPageSize is the tags-per-request hint.
//
// Large enough that a typical repository is one or two round trips, small
// enough that a repository with thousands of tags does not build a huge
// response in registry memory.
const defaultPageSize = 200

// maxTagResponseBytes bounds a tag-list response.
//
// A registry answering with something enormous — or a proxy returning an HTML
// error page — must not be able to exhaust memory during a routine scan.
const maxTagResponseBytes = 8 << 20 // 8 MiB

// ListTags returns one page of tags plus the token for the next.
//
// Pagination follows RFC 8288 Link headers, which is what Distribution
// specifies. `last` is the resume token from a previous call.
func (r *Repository) ListTags(ctx context.Context, last string, limit int) ([]string, string, error) {
	if limit <= 0 {
		limit = defaultPageSize
	}

	endpoint := r.url("/tags/list")
	q := url.Values{}
	q.Set("n", itoa(limit))
	if last != "" {
		q.Set("last", last)
	}
	endpoint += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", r.wrap("list tags", "", 0, nil, transportError(err))
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		// A repository with no tags yet is a normal state, not a failure —
		// some registries 404 rather than returning an empty list. Reporting
		// it as an error would make an empty repository look broken and would
		// back discovery off for no reason.
		if resp.StatusCode == http.StatusNotFound {
			return nil, "", nil
		}
		return nil, "", r.wrap("list tags", "", resp.StatusCode, resp, registry.ClassifyStatus(resp.StatusCode))
	}

	var payload struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTagResponseBytes)).Decode(&payload); err != nil {
		return nil, "", r.wrap("list tags", "", resp.StatusCode, nil, registry.ErrMalformedResponse)
	}

	return payload.Tags, nextFromLink(resp.Header.Get("Link")), nil
}

// nextFromLink extracts the `last` parameter from a rel="next" Link header.
//
// The header carries a URL, but only its `last` parameter is portable: some
// registries return a relative path, some an absolute URL, and some a
// differently-hosted one behind a proxy. Re-deriving the request URL ourselves
// and carrying only the cursor avoids following a link to somewhere we did not
// intend to go.
func nextFromLink(header string) string {
	if header == "" {
		return ""
	}

	for _, link := range strings.Split(header, ",") {
		link = strings.TrimSpace(link)
		if !strings.Contains(strings.ToLower(link), `rel="next"`) &&
			!strings.Contains(strings.ToLower(link), "rel=next") {
			continue
		}

		start := strings.Index(link, "<")
		end := strings.Index(link, ">")
		if start < 0 || end <= start {
			continue
		}

		target := link[start+1 : end]
		// Parse as a possibly-relative reference; only the query matters.
		u, err := url.Parse(target)
		if err != nil {
			continue
		}
		if lastVal := u.Query().Get("last"); lastVal != "" {
			return lastVal
		}
	}
	return ""
}

// ListAllTags walks every page.
//
// Discovery uses this: a full scan with no cursor is the design (docs/design/07
// §3), because the OCI tag list has no ordering guarantee and no change feed,
// so any incremental scheme would need a full scan to reconcile anyway.
//
// maxPages bounds a pathological or malicious registry that returns a `next`
// cursor forever; without it a scan could loop indefinitely holding a
// discovery slot.
func ListAllTags(ctx context.Context, lister registry.TagLister, maxPages int) ([]string, error) {
	if maxPages <= 0 {
		maxPages = 1000
	}

	var (
		all  []string
		last string
		seen = map[string]bool{}
	)

	for page := 0; page < maxPages; page++ {
		tags, next, err := lister.ListTags(ctx, last, defaultPageSize)
		if err != nil {
			return nil, err
		}

		for _, t := range tags {
			// A registry that repeats a tag across pages — which happens when
			// tags are written during a scan — must not produce duplicates.
			if !seen[t] {
				seen[t] = true
				all = append(all, t)
			}
		}

		if next == "" || next == last {
			// No next page, or a cursor that did not advance. The second case
			// would otherwise be an infinite loop.
			return all, nil
		}
		last = next
	}

	return all, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
