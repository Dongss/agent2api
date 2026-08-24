package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/config"
	"github.com/Dongss/agent2api/internal/ir"
)

// probeTimeout bounds each adapter's health check; a hung CLI must not hang the
// whole report.
const probeTimeout = 90 * time.Second

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that every configured agent CLI is installed and usable",
		Long: "Probe each configured adapter — binary on PATH, supported flags, login state —\n" +
			"and print a table. Backends that pass are the ones `serve` will offer.\n" +
			"Exits non-zero only if nothing at all is usable.\n\n" +
			"No prompt is sent, so running doctor costs nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runDoctor(cmd) },
	}
}

type doctorRow struct {
	id      string
	status  string
	binary  string
	version string
	account string
	models  string
	notes   []string
}

func runDoctor(cmd *cobra.Command) error {
	cfg, prov, log, closer, err := setup(cmd)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer.Close()
	}

	// Only the config file is echoed: doctor opens no socket, and printing a
	// listen address here read as if it had.
	out := cmd.OutOrStdout()
	if prov.File != "" {
		fmt.Fprintf(out, "config file: %s\n\n", prov.File)
	} else {
		fmt.Fprintf(out, "config file: none (using built-in defaults)\n\n")
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	rows := make([]doctorRow, 0, len(config.KnownAdapters))
	for _, id := range config.KnownAdapters {
		rows = append(rows, probeAdapter(ctx, cfg, id, log))
	}
	printDoctorTable(out, rows)

	if !servesAnything(rows) {
		return errors.New("no agent CLI is usable on this machine, so `serve` would have nothing to offer")
	}
	return nil
}

func probeAdapter(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) doctorRow {
	row := doctorRow{id: id, binary: "-", version: "-", account: "-", models: "-"}

	a, ok := cfg.Adapters[id]
	if !ok {
		row.status = "unknown"
		return row
	}
	row.binary = a.Binary
	row.models = id + ", " + id + ":<model>"

	if !adapter.IsRegistered(id) {
		row.status = "unavailable"
		row.notes = append(row.notes, "no adapter with this id is compiled into this build")
		return row
	}

	built, err := adapter.Build(id, adapter.Options{
		ID:             id,
		Config:         a,
		ScratchRoot:    cfg.Server.ScratchDir,
		RequestTimeout: cfg.Server.RequestTimeout.Duration(),
		IdleTimeout:    cfg.Server.IdleTimeout.Duration(),
		Logger:         log,
	})
	if err != nil {
		row.status = "error"
		row.notes = append(row.notes, err.Error())
		return row
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	health, err := built.Probe(probeCtx)
	if health.Binary != "" {
		row.binary = health.Binary
	}
	if health.Version != "" {
		row.version = health.Version
	}
	if health.Account != "" {
		row.account = health.Account
	}
	row.notes = append(row.notes, health.Notes...)

	if err != nil {
		row.status = "unavailable"
		e := ir.AsError(err)
		note := e.Message
		if e.Detail != "" {
			note += ": " + firstLines(e.Detail, 3)
		}
		row.notes = append(row.notes, note)
		return row
	}
	row.status = "ok"
	return row
}

func printDoctorTable(out io.Writer, rows []doctorRow) {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ADAPTER\tSTATUS\tBINARY\tVERSION\tACCOUNT\tMODELS")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			row.id, row.status, row.binary, row.version, row.account, row.models)
	}
	_ = tw.Flush()

	for _, row := range rows {
		for _, note := range row.notes {
			fmt.Fprintf(out, "\n%s: %s", row.id, note)
		}
	}
	if hasNotes(rows) {
		fmt.Fprintln(out)
	}
}

// servesAnything reports whether at least one backend probed clean.
func servesAnything(rows []doctorRow) bool {
	for _, row := range rows {
		if row.status == "ok" {
			return true
		}
	}
	return false
}

func hasNotes(rows []doctorRow) bool {
	for _, row := range rows {
		if len(row.notes) > 0 {
			return true
		}
	}
	return false
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, " / ")
}
