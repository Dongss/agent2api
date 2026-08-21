package main

import (
	"github.com/spf13/cobra"

	"github.com/Dongss/agent2api/internal/server"
)

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP gateway",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runServe(cmd) },
	}
}

func runServe(cmd *cobra.Command) error {
	cfg, _, log, closer, err := setup(cmd)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer.Close()
	}

	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()
	return srv.ListenAndServe(ctx)
}
