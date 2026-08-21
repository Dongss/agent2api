package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Dongss/agent2api/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the effective configuration",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "print",
		Short: "Print the merged config and where each value came from",
		Long: "Print the effective configuration, one key per line, annotated with the\n" +
			"layer that supplied it: default, file or flag. Bearer keys are redacted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, prov, err := config.Load(cmd.Flags())
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), prov.Report())
			return nil
		},
	})
	return cmd
}
