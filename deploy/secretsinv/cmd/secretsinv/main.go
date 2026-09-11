// Command secretsinv writes the local credential layout from
// config/secrets/secrets.yaml.
//
//	go run ./deploy/secretsinv/cmd/secretsinv
//	task secrets:scaffold
//
// It creates one directory per declared secret and one EMPTY file per key,
// which is the same shape a projected Kubernetes Secret volume produces. Fill
// them in with printf rather than echo:
//
//	printf '%s' 'svc-account' > config/secrets/local/internal-registry/username
//
// echo appends a newline, and a newline inside a password is a 401 nobody
// can see.
package main

import (
	"fmt"
	"os"

	"github.com/abhijeet-oxide/softwareGateway/deploy/secretsinv"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	created, err := secretsinv.Scaffold(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "secretsinv:", err)
		os.Exit(1)
	}
	if len(created) == 0 {
		fmt.Println("every declared credential already has a file under " + secretsinv.LocalDir)
		return
	}
	fmt.Printf("created %d empty files under %s:\n", len(created), secretsinv.LocalDir)
	for _, c := range created {
		fmt.Println("  " + c)
	}
	fmt.Println()
	fmt.Println("Fill them with printf, not echo - echo appends a newline and a newline")
	fmt.Println("inside a password is a 401 nobody can see.")
}
