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
	root.PersistentFlags().DurationVar(&opts.timeout, "timeout", 30*time.Second,
		"per-request timeout")

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
		os.Exit(exitCodeFor(err))
	}
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
	if errors.Is(err, v1.ErrUnreachable) {
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
