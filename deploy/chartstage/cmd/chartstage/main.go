// Command chartstage copies config/ and the deployment machinery the chart
// needs into deploy/charts/software-gateway/files.
//
// Run from the repository root:
//
//	go run ./deploy/chartstage/cmd/chartstage
//	task chart:stage
//
// The staged tree is not committed. See the package comment for why the copy
// exists at all, and deploy/deploy_test.go for the test that catches a stale one.
package main

import (
	"fmt"
	"os"

	"github.com/abhijeet-oxide/softwareGateway/deploy/chartstage"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	if err := chartstage.Stage(root); err != nil {
		fmt.Fprintln(os.Stderr, "chartstage:", err)
		os.Exit(1)
	}
	fmt.Printf("staged %d sources into %s\n", len(chartstage.Sources), chartstage.ChartFilesDir)
}
