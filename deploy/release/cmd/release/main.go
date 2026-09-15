// Command release prints what this product consists of, so a workflow can read
// the list rather than restate it.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/deploy/release"
)

func main() {
	what := "matrix"
	if len(os.Args) > 1 {
		what = os.Args[1]
	}
	comps := release.Components()

	switch what {
	case "matrix":
		// The `include` list for the images job, so the matrix is derived from
		// the same place the Build Info is.
		rows := make([]map[string]string, 0, len(comps))
		for _, c := range comps {
			rows = append(rows, map[string]string{
				"component":  c.Name,
				"dockerfile": c.Dockerfile,
			})
		}
		out, err := json.Marshal(rows)
		if err != nil {
			fmt.Fprintln(os.Stderr, "release:", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
	case "names":
		fmt.Println(strings.Join(release.Names(), ","))
	case "digest":
		// One component's inputs digest, for the workflow to label an image
		// with and to compare a published one against.
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "release digest: name a component")
			os.Exit(1)
		}
		for _, c := range comps {
			if c.Name != os.Args[2] {
				continue
			}
			d, err := release.InputsDigest(os.Getenv("GITHUB_SHA"), c)
			if err != nil {
				fmt.Fprintln(os.Stderr, "release:", err)
				os.Exit(1)
			}
			fmt.Println(d)
			return
		}
		fmt.Fprintf(os.Stderr, "release digest: no component named %q\n", os.Args[2])
		os.Exit(1)
	case "inputs":
		for _, c := range comps {
			fmt.Printf("%s\t%s\n", c.Name, strings.Join(c.Inputs, " "))
		}
	default:
		fmt.Fprintf(os.Stderr, "release: unknown request %q; want matrix, names, inputs or digest\n", what)
		os.Exit(1)
	}
}
