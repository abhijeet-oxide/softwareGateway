package buildinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one Artifactory.
type Client struct {
	// BaseURL is the Artifactory root, e.g. https://artifacts.example.internal/artifactory
	BaseURL  string
	Username string
	Token    string
	HTTP     *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s: %w", path, err)
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.BaseURL, "/")+path, buf)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Basic, because that is what an Artifactory access token is used as. The
	// credential is never a query parameter: those are logged by proxies.
	req.SetBasicAuth(c.Username, c.Token)

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer res.Body.Close() //nolint:errcheck // response body close on a request that already answered

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	// Bounded: an Artifactory error page can be a megabyte of HTML, and the
	// useful part is at the front.
	detail, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s %s: %s - the credential reached Artifactory and was refused. "+
			"Publishing Build Info needs deploy and read permission on the build, which is "+
			"separate from permission to push the images.\n%s", method, path, res.Status, detail)
	case http.StatusNotFound:
		return fmt.Errorf("%s %s: %s - check BUILD_INFO_URL names the Artifactory root, "+
			"including /artifactory.\n%s", method, path, res.Status, detail)
	default:
		return fmt.Errorf("%s %s: %s\n%s", method, path, res.Status, detail)
	}
}

// Publish writes the Build Info. Artifactory answers 204 with no body.
func (c *Client) Publish(ctx context.Context, bi *BuildInfo) error {
	return c.do(ctx, http.MethodPut, "/api/build", bi)
}

// Promotion moves a build's artifacts between repositories.
type Promotion struct {
	Status     string `json:"status"`
	Comment    string `json:"comment,omitempty"`
	CIUser     string `json:"ciUser,omitempty"`
	SourceRepo string `json:"sourceRepo"`
	TargetRepo string `json:"targetRepo"`
	// Copy rather than move: a promotion that empties the staging repository
	// takes the release away from anything still pulling it.
	Copy bool `json:"copy"`
	// Artifacts without dependencies: the build's own output moves, not the
	// third-party images it was built from, which belong to their own repos.
	Artifacts    bool `json:"artifacts"`
	Dependencies bool `json:"dependencies"`
	FailFast     bool `json:"failFast"`
}

// Promote moves one build to another repository. It is a separate call from
// Publish and a separate decision: a build is published because it was built,
// and promoted because somebody decided it was good.
func (c *Client) Promote(ctx context.Context, name, number string, p Promotion) error {
	if p.TargetRepo == "" {
		return fmt.Errorf("promotion needs a target repository")
	}
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/build/promote/%s/%s", name, number), p)
}
