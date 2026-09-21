package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Dongss/agent2api/internal/config"
	"github.com/Dongss/agent2api/internal/logging"
	"github.com/Dongss/agent2api/internal/version"
)

const rootLong = `agent2api exposes the agent CLIs installed on this machine
(Claude Code, Codex, Cursor, Qwen Code) as OpenAI- and Anthropic-compatible
HTTP APIs.

Point any OpenAI SDK at the gateway's base URL and requests are translated into
a local CLI invocation. Every request is independent: the whole conversation is
replayed into a fresh CLI process, with the CLI's own tools disabled.

Start the gateway with "agent2api serve"; "agent2api doctor" reports which agent
CLIs this machine can serve, and "agent2api update" updates agent2api to the
latest version.`

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "agent2api",
		Short:         "Serve local agent CLIs as OpenAI- and Anthropic-compatible APIs",
		Long:          rootLong,
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		// Running the bare command prints help rather than starting a server:
		// a gateway that begins accepting requests because someone typed the
		// binary's name is a surprise worth not having. Starting it is spelled
		// `agent2api serve`.
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
		Args: cobra.NoArgs,
	}

	// Configuration flags are global: they apply to serve, doctor and
	// config print alike.
	config.RegisterFlags(root.PersistentFlags())
	// The version leads the help output: the bare command is what someone runs
	// to see what they have, and "which build is this?" is part of that.
	root.SetHelpTemplate("agent2api " + version.String() + "\n\n" +
		root.HelpTemplate() + "\n" + config.ConfigFileHelp() + "\n")

	root.AddCommand(newServeCmd(), newDoctorCmd(), newConfigCmd(), newUpdateCmd(), newVersionCmd())
	return root
}

// setup loads the effective configuration and builds the logger. Every command
// starts here, so `doctor` and `serve` always agree on what the config is.
func setup(cmd *cobra.Command) (*config.Config, *config.Provenance, *slog.Logger, io.Closer, error) {
	// cmd.Flags() is the post-parse set: local flags plus the inherited
	// persistent ones that carry every config key.
	cfg, prov, err := config.Load(cmd.Flags())
	if err != nil {
		return nil, nil, nil, nil, err
	}
	log, closer, err := logging.New(cfg.Log)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return cfg, prov, log, closer, nil
}

// signalContext cancels on SIGINT/SIGTERM so shutdown can drain in flight work.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the agent2api version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "agent2api "+version.String())
			return nil
		},
	}
}
