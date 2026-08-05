// Command transferctl is the softwareGateway CLI.
//
// INVARIANT: transferctl is a pure Coordinator API client. It never contacts a
// registry, never opens a database connection, and never talks to a worker.
// That is what keeps one audit chokepoint and makes the binary safe to hand to
// anyone — it can do nothing a user could not do with curl.
//
// The one exception is `config validate`, which is deliberately offline: it
// runs the same validator the Coordinator runs, in CI, before merge.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Exit codes. Distinct enough to be scripted against — see
// docs/design/13-cli.md section 1.
const (
	exitOK             = 0
	exitError          = 1
	exitUsage          = 2
	exitUnreachable    = 3
	exitNotFound       = 4
	exitPrecondition   = 5
	exitPartialFailure = 6
)

// Request timeouts. Two defaults, because the commands are not the same kind
// of operation.
//
// Most of what transferctl does is a database read behind an HTTP handler, and
// 30 seconds is generous. But `products check` opens TLS connections to every
// vendor registry a product declares, and `packages discover` runs a full scan
// — through a corporate proxy, against a registry on the other side of a WAN
// link, both are routinely minutes. One shared 30-second default made those
// two commands fail almost every time, and fail with
//
//	coordinator unreachable: context deadline exceeded (Client.Timeout exceeded
//	while awaiting headers)
//
// which blames the Coordinator for being slow to answer a question that is
// genuinely slow to answer.
const (
	defaultTimeout = 30 * time.Second
	// slowTimeout applies to commands that reach third-party registries. Not
	// unlimited: a hung connection should still end in an error rather than a
	// terminal that never returns.
	slowTimeout = 10 * time.Minute
)

type globalOptions struct {
	endpoint string
	output   string
	token    string
	timeout  time.Duration
}

var opts globalOptions

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "transferctl",
		Short: "Control softwareGateway package transfers",
		Long: "transferctl is the command-line client for softwareGateway.\n\n" +
			"It communicates only with the Coordinator API — never directly with\n" +
			"a registry, a worker, or the database.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Precedence is flag -> SWGW_ env -> default. Config-file support lands
	// with the commands that need more than an endpoint.
	root.PersistentFlags().StringVar(&opts.endpoint, "endpoint",
		envOr("SWGW_ENDPOINT", "http://localhost:8080"), "Coordinator API endpoint")
	root.PersistentFlags().StringVarP(&opts.output, "output", "o",
		envOr("SWGW_OUTPUT", "table"), "output format: table|json|yaml")
	root.PersistentFlags().StringVar(&opts.token, "token",
		os.Getenv("SWGW_TOKEN"),
		"bearer token (accepted now; the Coordinator is unauthenticated in v1)")
	root.PersistentFlags().DurationVar(&opts.timeout, "timeout",
		envDurationOr("SWGW_TIMEOUT", defaultTimeout),
		"per-request timeout (raised automatically for commands that contact registries)")

	root.AddCommand(
		newVersionCommand(),
		newHealthCommand(),
		newProductsCommand(),
		newPackagesCommand(),
		newConfigCommand(),
	)
	return root
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		if hint := hintFor(err); hint != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n", hint)
		}
		os.Exit(exitCodeFor(err))
	}
}

// hintFor adds the next step for failures whose message does not imply one.
//
// A timeout is the case that matters. The underlying message says the deadline
// was exceeded while awaiting headers, which reads like a broken server and is
// usually a slow one — so say which knob turns, since nobody guesses a flag
// they have not needed before.
func hintFor(err error) string {
	if errors.Is(err, v1.ErrTimeout) {
		return "The Coordinator accepted the connection but had not answered yet. If the\n" +
			"work is genuinely slow — a check across many registries, or a scan of a\n" +
			"large one — raise the deadline:\n\n" +
			"  transferctl --timeout 15m ...\n" +
			"  SWGW_TIMEOUT=15m transferctl ...\n\n" +
			"The scan itself keeps running on the Coordinator; only this client stopped\n" +
			"waiting for the answer."
	}
	return ""
}

// exitCodeFor maps an error to an exit code.
//
// The distinction that matters for CI: "the Coordinator is down" (3) is not
// the same as "you asked for something that does not exist" (4), and neither
// is a generic failure.
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	// A timeout shares exit code 3 with an unreachable Coordinator: from a
	// script's point of view both mean "no answer came back", and splitting them
	// would break pipelines that already branch on 3. The MESSAGE is where they
	// differ, and the message is what a human acts on.
	if errors.Is(err, v1.ErrUnreachable) || errors.Is(err, v1.ErrTimeout) {
		return exitUnreachable
	}

	var p *v1.Problem
	if errors.As(err, &p) {
		switch p.Code {
		case v1.CodeNotFound:
			return exitNotFound
		case v1.CodeFailedPrecondition, v1.CodeAborted:
			return exitPrecondition
		}
	}

	var pf partialFailureError
	if errors.As(err, &pf) {
		return exitPartialFailure
	}
	return exitError
}

// partialFailureError signals that an operation completed with failures — a
// degraded health check, or a watched transfer that ended in FAILED. CI must
// see a non-zero exit for these, or a pipeline reports green on a failed
// replication.
type partialFailureError struct{ msg string }

func (e partialFailureError) Error() string { return e.msg }

func newClient() *v1.Client {
	return v1.NewClient(opts.endpoint,
		v1.WithToken(opts.token),
		v1.WithTimeout(opts.timeout),
	)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envDurationOr reads a duration from the environment.
//
// An unparseable value falls back to the default rather than failing: a bad
// SWGW_TIMEOUT should not stop `transferctl version` from working, and the
// resolved value is visible in --help.
func envDurationOr(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// contactsRegistries raises the timeout for a command that talks to third-party
// registries through the Coordinator.
//
// Applied only when the operator has NOT chosen a timeout themselves, by flag
// or by SWGW_TIMEOUT. An explicit `--timeout 5s` means five seconds, including
// on a slow command — quietly overriding it would make the flag a suggestion.
func contactsRegistries(cmd *cobra.Command) {
	cmd.PreRun = func(c *cobra.Command, _ []string) {
		if c.Flags().Changed("timeout") || strings.TrimSpace(os.Getenv("SWGW_TIMEOUT")) != "" {
			return
		}
		opts.timeout = slowTimeout
	}
}
