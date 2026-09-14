// Command chartstage copies config/ and the deployment machinery the chart
// needs into deploy/charts/software-gateway/files, and answers the two other
// questions a developer and the pipeline both have about a deployment.
//
// Run from the repository root:
//
//	go run ./deploy/chartstage/cmd/chartstage                     stage the chart
//	go run ./deploy/chartstage/cmd/chartstage -values lab         print an instance's values
//	go run ./deploy/chartstage/cmd/chartstage -instances          list the instances
//	go run ./deploy/chartstage/cmd/chartstage -instance lab -set-version 1.4.3
//
// The staged tree is not committed. See the package comment for why the copy
// exists at all, and deploy/deploy_test.go for the test that catches a stale one.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/deploy/chartstage"
)

func main() {
	root := flag.String("root", ".", "repository root")
	values := flag.String("values", "", "print this instance's values file and exit")
	list := flag.Bool("instances", false, "list the deployment instances and the version each runs")
	instance := flag.String("instance", "", "the instance -set-version applies to")
	setVersion := flag.String("set-version", "", "point -instance at this chart version, creating the patch directory if needed")
	flag.Parse()

	if err := run(*root, *values, *list, *instance, *setVersion); err != nil {
		fmt.Fprintln(os.Stderr, "chartstage:", err)
		os.Exit(1)
	}
}

func run(root, values string, list bool, instance, setVersion string) error {
	switch {
	case values != "":
		b, err := chartstage.InstanceValues(root, values)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err

	case list:
		names, err := chartstage.Instances(root)
		if err != nil {
			return err
		}
		for _, name := range names {
			version, err := chartstage.InstanceVersion(root, name)
			if err != nil {
				return err
			}
			fmt.Printf("%-12s %s\n", name, version)
		}
		return nil

	case setVersion != "":
		if instance == "" {
			return fmt.Errorf("-set-version needs -instance")
		}
		if err := chartstage.SetInstanceVersion(root, instance, strings.TrimPrefix(setVersion, "v")); err != nil {
			return err
		}
		fmt.Printf("%s now runs %s\n", instance, setVersion)
		return nil

	default:
		if err := chartstage.Stage(root); err != nil {
			return err
		}
		if err := chartstage.StageSchema(root); err != nil {
			return err
		}
		fmt.Printf("staged %d sources into %s\n", len(chartstage.Sources), chartstage.ChartFilesDir)
		return nil
	}
}
