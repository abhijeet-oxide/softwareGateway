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
	case "inputs":
		for _, c := range comps {
			fmt.Printf("%s\t%s\n", c.Name, strings.Join(c.Inputs, " "))
		}
	default:
		fmt.Fprintf(os.Stderr, "release: unknown request %q; want matrix, names or inputs\n", what)
		os.Exit(1)
	}
}
