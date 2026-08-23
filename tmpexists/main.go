package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var user, pass string

func main() {
	user = read("./dev/secrets/cfx-jfrog-secret/username")
	pass = read("./dev/secrets/cfx-jfrog-secret/password")

	// One digest Xray says is not indexed, and one it scanned.
	sums := os.Args[1:]

	for _, s := range sums {
		hex := strings.TrimPrefix(s, "sha256:")
		code, raw := do("GET", "/artifactory/api/search/checksum?sha256="+hex+"&repos=apm0014228-oci-stage", nil, "")
		fmt.Printf("=== checksum search %s -> %d\n", hex[:12], code)
		var out struct {
			Results []struct {
				URI string `json:"uri"`
			} `json:"results"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			fmt.Println("   ", truncate(string(raw), 300))
			continue
		}
		fmt.Println("    results:", len(out.Results))
		for i, r := range out.Results {
			if i >= 2 {
				break
			}
			fmt.Println("     ", r.URI)
		}
	}

	// And the same question asked for the whole batch at once.
	var clauses []string
	for _, s := range sums {
		clauses = append(clauses, fmt.Sprintf(`{"actual_sha256":{"$eq":"%s"}}`, strings.TrimPrefix(s, "sha256:")))
	}
	aql := fmt.Sprintf(`items.find({"repo":{"$eq":"apm0014228-oci-stage"},"$or":[%s]}).include("actual_sha256")`,
		strings.Join(clauses, ","))
	code, raw := do("POST", "/artifactory/api/search/aql", []byte(aql), "text/plain")
	fmt.Printf("=== aql -> %d\n", code)
	fmt.Println("   ", truncate(string(raw), 900))
}

func do(method, path string, body []byte, contentType string) (int, []byte) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, "https://artifact.it.att.com"+path, r)
	req.SetBasicAuth(user, pass)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(b))
}
