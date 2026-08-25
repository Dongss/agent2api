package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dongss/agent2api/internal/selfupdate"
	"github.com/Dongss/agent2api/internal/version"
)

// updateTimeout bounds the whole operation. A release archive is tens of
// megabytes, so this is generous on purpose: the failure worth guarding against
// is a stalled connection, not a slow one.
const updateTimeout = 10 * time.Minute

func newUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update agent2api to the latest version",
		Long: "Download the latest agent2api release for this platform, verify its SHA-256\n" +
			"against the published checksums, and swap it in place of the running binary.\n\n" +
			"Nothing on disk changes until the download has been verified, so a network\n" +
			"failure or a bad checksum leaves the current version exactly as it was.\n\n" +
			"A gateway already running under `agent2api serve` keeps the old version until\n" +
			"it is restarted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runUpdate(cmd) },
	}
}

// runUpdate deliberately skips setup(): updating is not a reason to need a
// loadable config, and a typo in the config file should not be able to block
// installing the fix for it.
func runUpdate(cmd *cobra.Command) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	out := cmd.OutOrStdout()
	current := version.String()
	fmt.Fprintf(out, "agent2api %s: checking for a newer release…\n", current)

	res, err := selfupdate.Run(ctx, selfupdate.Options{Current: current})
	if err != nil {
		return err
	}
	if !res.Updated {
		fmt.Fprintf(out, "already on the latest release (%s)\n", res.To)
		return nil
	}
	fmt.Fprintf(out, "updated %s → %s\n", res.From, res.To)
	fmt.Fprintf(out, "installed %s\n", res.Path)
	fmt.Fprintln(out, "a running `agent2api serve` keeps the old version until it is restarted")
	return nil
}
