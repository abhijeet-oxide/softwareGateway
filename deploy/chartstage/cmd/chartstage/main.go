// Command chartstage copies config/ and the deployment machinery the chart
// needs into deploy/charts/software-gateway/files.
//
// Run from the repository root:
//
//	go run ./deploy/chartstage/cmd/chartstage
//	task chart:stage
//
// It also prints one environment's Helm values, which is the other thing both
// a developer and the pipeline need and neither should reimplement:
//
//	go run ./deploy/chartstage/cmd/chartstage -values nprd
//	task chart:template -- nprd
//
// The staged tree is not committed. See the package comment for why the copy
// exists at all, and deploy/deploy_test.go for the test that catches a stale one.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/abhijeet-oxide/softwareGateway/deploy/chartstage"
)

func main() {
	values := flag.String("values", "", "print this environment's spec.values from its HelmRelease and exit (lab, prod)")
	root := flag.String("root", ".", "repository root")
	flag.Parse()

	if *values != "" {
		b, err := chartstage.EnvironmentValues(*root, *values)
		if err != nil {
			fmt.Fprintln(os.Stderr, "chartstage:", err)
			os.Exit(1)
		}
		if _, err := os.Stdout.Write(b); err != nil {
			fmt.Fprintln(os.Stderr, "chartstage:", err)
			os.Exit(1)
		}
		return
	}

	if err := chartstage.Stage(*root); err != nil {
		fmt.Fprintln(os.Stderr, "chartstage:", err)
		os.Exit(1)
	}
	fmt.Printf("staged %d sources into %s\n", len(chartstage.Sources), chartstage.ChartFilesDir)
}
