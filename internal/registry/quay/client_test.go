package quay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := New(Config{Endpoint: srv.URL, Token: "tok", RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func TestTokenIsRequired(t *testing.T) {
	if _, err := New(Config{Endpoint: "https://quay.example.com"}); err == nil {
		t.Fatal("a client with no token must not be constructible")
	} else if !errors.Is(err, registry.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestEndpointNormalisation(t *testing.T) {
	cases := map[string]string{
		"quay.example.com":          "https://quay.example.com",
		"https://quay.example.com":  "https://quay.example.com",
		"https://quay.example.com/": "https://quay.example.com",
		"http://quay:8080":          "http://quay:8080",
	}
	for in, want := range cases {
		c, err := New(Config{Endpoint: in, Token: "t"})
		if err != nil {
			t.Fatalf("New(%q): %v", in, err)
		}
		if c.Endpoint() != want {
			t.Fatalf("New(%q).Endpoint() = %q, want %q", in, c.Endpoint(), want)
		}
	}
	if _, err := New(Config{Endpoint: "", Token: "t"}); err == nil {
		t.Fatal("an empty endpoint must be rejected")
	}
}

// The bearer token goes on every request. A robot account here gets a 403
// that reads like a permissions problem, so getting this wrong is expensive.
func TestSendsBearerToken(t *testing.T) {
	var got string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{}`)
	})
	if _, err := c.GetRepository(context.Background(), "org", "repo"); err != nil {
		t.Fatalf("GetRepository: %v", err)
	}
	if got != "Bearer tok" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer tok")
	}
}

func TestGetMirrorParsesQuaysDocument(t *testing.T) {
	const body = `{
	  "is_enabled": true,
	  "external_reference": "artifactory.example.com/docker-local/vendor-a",
	  "external_registry_username": "reader",
	  "sync_start_date": "2026-09-01T02:00:00Z",
	  "sync_interval": 21600,
	  "robot_username": "platform-prod+swgw-mirror",
	  "root_rule": {"rule_kind": "tag_glob_csv", "rule_value": ["v3.*", "sha256-*.sig"]},
	  "external_registry_config": {"verify_tls": true, "unsigned_images": false,
	     "proxy": {"https_proxy": "http://proxy:3128", "no_proxy": "a.example.com,b.example.com"}},
	  "skopeo_timeout_interval": 300,
	  "sync_status": "SUCCESS",
	  "sync_expiration_date": "2026-09-01T08:00:00Z",
	  "sync_retries_remaining": 3,
	  "mirror_type": "PULL"
	}`
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repository/platform-prod/vendor-a-platform/mirror" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, body)
	})

	got, err := c.GetMirror(context.Background(), "platform-prod", "vendor-a-platform")
	if err != nil {
		t.Fatalf("GetMirror: %v", err)
	}
	if got.Status() != SyncSuccess {
		t.Fatalf("status = %q", got.Status())
	}
	if got.SyncInterval != 21600 || got.RobotUsername != "platform-prod+swgw-mirror" {
		t.Fatalf("unexpected state: %+v", got)
	}
	if len(got.RootRule.RuleValue) != 2 || got.RootRule.RuleValue[1] != "sha256-*.sig" {
		t.Fatalf("root rule = %+v", got.RootRule)
	}
	if got.ExternalRegistryConfig == nil || got.ExternalRegistryConfig.Proxy == nil ||
		got.ExternalRegistryConfig.Proxy.NoProxy != "a.example.com,b.example.com" {
		t.Fatalf("external registry config = %+v", got.ExternalRegistryConfig)
	}
}

// An unrecognised status must stay unrecognised. Mapping it onto SUCCESS or
// FAIL would answer delivery-plan Q7 by guessing.
func TestUnknownSyncStatusIsNotGuessed(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"sync_status": "QUIESCING"}`)
	})
	got, err := c.GetMirror(context.Background(), "org", "repo")
	if err != nil {
		t.Fatalf("GetMirror: %v", err)
	}
	if got.Status() != SyncUnknown {
		t.Fatalf("status = %q, want UNKNOWN", got.Status())
	}
	if got.RawSyncStatus != "QUIESCING" {
		t.Fatalf("the raw value must survive so it can be reported verbatim, got %q", got.RawSyncStatus)
	}
	if got.Status().Terminal() || got.Status().Running() {
		t.Fatal("an unknown status is neither terminal nor running — we do not know")
	}
}

func TestSyncStatusPredicates(t *testing.T) {
	for _, s := range []SyncStatus{SyncSuccess, SyncFail, SyncNeverRun} {
		if !s.Terminal() || s.Running() {
			t.Fatalf("%q should be terminal and not running", s)
		}
	}
	for _, s := range []SyncStatus{SyncSyncing, SyncNow} {
		if s.Terminal() || !s.Running() {
			t.Fatalf("%q should be running and not terminal", s)
		}
	}
}

// A repository with no mirror answers 404, which is a normal negative answer.
func TestMissingMirrorIsNotFound(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"detail": "Not Found"}`)
	})
	_, err := c.GetMirror(context.Background(), "org", "repo")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// PutMirror picks POST or PUT for the caller, because Quay 4xxs the wrong one
// and no caller should have to know which.
func TestPutMirrorCreatesWhenAbsent(t *testing.T) {
	var methods []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	if err := c.PutMirror(context.Background(), "org", "repo", MirrorSpec{}); err != nil {
		t.Fatalf("PutMirror: %v", err)
	}
	if len(methods) != 2 || methods[1] != http.MethodPost {
		t.Fatalf("methods = %v, want a GET then a POST", methods)
	}
}

func TestPutMirrorUpdatesWhenPresent(t *testing.T) {
	var methods []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"is_enabled": true}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.PutMirror(context.Background(), "org", "repo", MirrorSpec{}); err != nil {
		t.Fatalf("PutMirror: %v", err)
	}
	if len(methods) != 2 || methods[1] != http.MethodPut {
		t.Fatalf("methods = %v, want a GET then a PUT", methods)
	}
}

func TestPutMirrorSendsQuaysFieldNames(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
	})

	verify := true
	spec := MirrorSpec{
		IsEnabled: true, ExtRef: "artifactory.example.com/docker-local",
		SyncStartDate: "2026-09-01T02:00:00Z", SyncInterval: 21600,
		RobotUsername:         "platform-prod+swgw",
		RootRule:              RootRule{RuleKind: RuleKindTagGlobCSV, RuleValue: []string{"v3.*"}},
		SkopeoTimeoutInterval: 300,
		ExternalRegistryConfig: &ExternalRegistryConfig{
			VerifyTLS: &verify,
			Proxy:     &ProxyConfig{HTTPSProxy: "http://proxy:3128", NoProxy: "a,b"},
		},
	}
	if err := c.PutMirror(context.Background(), "org", "repo", spec); err != nil {
		t.Fatalf("PutMirror: %v", err)
	}

	for _, key := range []string{
		"is_enabled", "external_reference", "sync_start_date", "sync_interval",
		"robot_username", "root_rule", "external_registry_config", "skopeo_timeout_interval",
	} {
		if _, ok := body[key]; !ok {
			t.Fatalf("field %q missing from the request body: %v", key, body)
		}
	}
	rule, _ := body["root_rule"].(map[string]any)
	if rule["rule_kind"] != RuleKindTagGlobCSV {
		t.Fatalf("rule_kind = %v", rule["rule_kind"])
	}
}

// A 409 on sync-now means a sync is already running, which is what the caller
// wanted. Reporting it as an error would make a correct outcome look broken.
func TestSyncNowTreats409AsAlreadyRunning(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"detail": "Mirroring is already in progress"}`)
	})
	running, err := c.SyncNow(context.Background(), "org", "repo")
	if err != nil {
		t.Fatalf("a 409 here is not a failure: %v", err)
	}
	if !running {
		t.Fatal("expected alreadyRunning")
	}
}

func TestSyncNowSurfacesRealFailures(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := c.SyncNow(context.Background(), "org", "repo"); !errors.Is(err, registry.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestCancelSyncTreats409AsNothingRunning(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	was, err := c.CancelSync(context.Background(), "org", "repo")
	if err != nil || was {
		t.Fatalf("want (false, nil), got (%v, %v)", was, err)
	}
}

// The 403 an operator will actually hit, and the sentence that saves the hour.
func TestForbiddenNamesTheCredentialMistake(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := c.GetMirror(context.Background(), "org", "repo")
	if !errors.Is(err, registry.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if !strings.Contains(err.Error(), "/v2") || !strings.Contains(err.Error(), "OAuth") {
		t.Fatalf("the message must explain that a robot cannot authenticate here, got %q", err)
	}
}

// Quay's own words, when it has any, beat ours.
func TestQuaysDetailIsPreserved(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail": "Invalid robot_username"}`)
	})
	_, err := c.GetMirror(context.Background(), "org", "repo")
	if err == nil || !strings.Contains(err.Error(), "Invalid robot_username") {
		t.Fatalf("got %v", err)
	}
}

// An ingress error page is not JSON. One line of it beats the whole page and
// beats nothing at all.
func TestNonJSONErrorBodyIsSummarised(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "<html>\n<title>502 Bad Gateway</title>\n<body>...</body>\n</html>")
	})
	_, err := c.GetMirror(context.Background(), "org", "repo")
	if err == nil || !strings.Contains(err.Error(), "<html>") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "<body>") {
		t.Fatalf("only the first line should be quoted, got %q", err)
	}
}

func TestSetRepositoryStateSendsState(t *testing.T) {
	var body map[string]string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/changestate") {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
	})
	if err := c.SetRepositoryState(context.Background(), "org", "repo", StateMirror); err != nil {
		t.Fatalf("SetRepositoryState: %v", err)
	}
	if body["state"] != "MIRROR" {
		t.Fatalf("state = %q", body["state"])
	}
}

func TestProxyCacheRoundTrip(t *testing.T) {
	var seen []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"upstream_registry":"artifactory.example.com/docker-local","expiration_s":86400,"org_name":"dev-cache"}`)
		}
	})
	ctx := context.Background()

	got, err := c.GetProxyCache(ctx, "dev-cache")
	if err != nil {
		t.Fatalf("GetProxyCache: %v", err)
	}
	if got.UpstreamRegistry != "artifactory.example.com/docker-local" || got.ExpirationS != 86400 {
		t.Fatalf("proxy cache = %+v", got)
	}
	if err := c.ValidateProxyCache(ctx, "dev-cache", *got); err != nil {
		t.Fatalf("ValidateProxyCache: %v", err)
	}
	if err := c.CreateProxyCache(ctx, "dev-cache", *got); err != nil {
		t.Fatalf("CreateProxyCache: %v", err)
	}
	if err := c.DeleteProxyCache(ctx, "dev-cache"); err != nil {
		t.Fatalf("DeleteProxyCache: %v", err)
	}

	want := []string{
		"GET /api/v1/organization/dev-cache/proxycache",
		"POST /api/v1/organization/dev-cache/validateproxycache",
		"POST /api/v1/organization/dev-cache/proxycache",
		"DELETE /api/v1/organization/dev-cache/proxycache",
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, seen[i], want[i])
		}
	}
}

func TestSplitRepository(t *testing.T) {
	cases := []struct {
		in, ns, name string
		ok           bool
	}{
		{"platform-prod/vendor-a-platform", "platform-prod", "vendor-a-platform", true},
		{"org/a/b", "org", "a/b", true},
		{"noSlash", "", "", false},
		{"trailing/", "", "", false},
		{"/leading", "", "", false},
	}
	for _, c := range cases {
		ns, name, ok := SplitRepository(c.in)
		if ns != c.ns || name != c.name || ok != c.ok {
			t.Fatalf("SplitRepository(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, ns, name, ok, c.ns, c.name, c.ok)
		}
	}
}

func TestSecondsOfRoundsUp(t *testing.T) {
	cases := map[time.Duration]int64{
		0: 0, -time.Second: 0,
		6 * time.Hour:           21600,
		800 * time.Millisecond:  1, // must not become "unset"
		1500 * time.Millisecond: 2,
	}
	for in, want := range cases {
		if got := SecondsOf(in); got != want {
			t.Fatalf("SecondsOf(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestParseTimeToleratesQuaysForms(t *testing.T) {
	for _, s := range []string{"2026-09-01T02:00:00Z", "2026-09-01T02:00:00", "2026-09-01 02:00:00"} {
		got, ok := ParseTime(s)
		if !ok || got.Year() != 2026 || got.Hour() != 2 {
			t.Fatalf("ParseTime(%q) = (%v, %v)", s, got, ok)
		}
	}
	if _, ok := ParseTime("never"); ok {
		t.Fatal("an unparseable timestamp must report failure rather than a zero time")
	}
}
