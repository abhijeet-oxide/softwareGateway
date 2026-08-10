package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/version"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func newVersionCommand() *cobra.Command {
	var clientOnly bool

	cmd := &cobra.Command{
		Use: "version",
		// Declared so a stray argument is refused rather than silently
		// ignored, which reads as the command having accepted it.
		Args:  cobra.NoArgs,
		Short: "Show client and server versions",
		Long: "Shows the transferctl build and, unless --client is given, the\n" +
			"Coordinator's build and applied schema version.\n\n" +
			"A version mismatch between the binary and the schema is the first\n" +
			"thing to check when behaviour does not match the documentation.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := version.Get("transferctl")

			if clientOnly {
				return render(stdout(), opts.output, client, func(w io.Writer) error {
					fmt.Fprintf(w, "Client   %s (%s)\n", client.Version, client.Commit)
					return nil
				})
			}

			server, err := newClient().Version(cmd.Context())
			if err != nil {
				// A version check must still tell you the client build when
				// the server is unreachable — that is often exactly what you
				// need in order to debug why it is unreachable.
				if errors.Is(err, v1.ErrUnreachable) {
					fmt.Fprintf(stdout(), "Client   %s (%s)\n", client.Version, client.Commit)
					fmt.Fprintf(stdout(), "Server   unreachable at %s\n", opts.endpoint)
				}
				return err
			}

			combined := struct {
				Client version.Info        `json:"client"`
				Server *v1.VersionResponse `json:"server"`
			}{client, server}

			return render(stdout(), opts.output, combined, func(w io.Writer) error {
				tw := newTabWriter(w)
				fmt.Fprintf(tw, "Client\t%s\t%s\t%s\n", client.Version, client.Commit, client.GoVersion)
				fmt.Fprintf(tw, "Server\t%s\t%s\t%s\n", server.Version, server.Commit, server.GoVersion)
				fmt.Fprintf(tw, "API\t%s\n", server.APIVersion)
				fmt.Fprintf(tw, "Schema\t%d\n", server.SchemaVersion)
				return tw.Flush()
			})
		},
	}

	cmd.Flags().BoolVar(&clientOnly, "client", false, "show only the client version")
	return cmd
}
