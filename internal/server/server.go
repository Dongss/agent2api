// Package server wires configuration into a running HTTP gateway: it builds
// the adapters, the router and the API frontends, and owns the listener.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"strings"

	"github.com/Dongss/agent2api/internal/adapter"
	"github.com/Dongss/agent2api/internal/config"
	"github.com/Dongss/agent2api/internal/frontend/anthropic"
	"github.com/Dongss/agent2api/internal/frontend/openai"
	"github.com/Dongss/agent2api/internal/frontend/responses"
	"github.com/Dongss/agent2api/internal/gate"
	"github.com/Dongss/agent2api/internal/ir"
	"github.com/Dongss/agent2api/internal/router"
	"github.com/Dongss/agent2api/internal/version"
)

// Skipped records a backend that was configured but is not being served —
// almost always a CLI that is not installed or not logged in on this machine.
type Skipped struct {
	ID     string
	Reason string
}

// Server is the agent2api HTTP gateway.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	router  *router.Router
	skipped []Skipped
	handler http.Handler
}

// New builds the gateway. Backends are auto-detected: each configured CLI is
// probed, and the ones that are installed and logged in start serving. The rest
// are reported in [Server.Skipped] rather than failing startup, so the gateway
// runs with whatever this machine happens to have.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	adapters, skipped, err := BuildAdapters(context.Background(), cfg, log)
	if err != nil {
		return nil, err
	}
	if len(adapters) == 0 {
		return nil, fmt.Errorf("no agent CLI is usable on this machine, so there is nothing to serve; "+
			"run `agent2api doctor` for the details (looked for: %s)",
			strings.Join(cfg.AdapterIDs(), ", "))
	}

	rt, err := router.New(adapters)
	if err != nil {
		return nil, err
	}

	s := &Server{cfg: cfg, log: log, router: rt, skipped: skipped}

	mux := http.NewServeMux()
	oai := openai.New(rt, log)
	oai.Heartbeat = cfg.Server.HeartbeatInterval.Duration()
	oai.Routes(mux)
	ant := anthropic.New(rt, log)
	ant.Heartbeat = cfg.Server.HeartbeatInterval.Duration()
	ant.Routes(mux)
	resp := responses.New(rt, log)
	resp.Heartbeat = cfg.Server.HeartbeatInterval.Duration()
	resp.Routes(mux)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("/", notFound)

	s.handler = chain(mux,
		recovery(log),
		requestLog(log),
		auth(cfg.Server.APIKey),
	)
	return s, nil
}

// ProbeTimeout bounds the health check of a single backend at startup, so one
// wedged CLI cannot hold up the listener.
const ProbeTimeout = 30 * time.Second

// BuildAdapters instantiates the backends this machine can actually serve.
//
// Every configured adapter is probed. A probe failure is a skip, not an error:
// having only some of these CLIs installed is the normal case, and a gateway
// that refuses to start because Cursor is absent would be useless. Startup only
// fails when nothing at all is usable, which [New] reports.
func BuildAdapters(ctx context.Context, cfg *config.Config, log *slog.Logger) ([]adapter.Adapter, []Skipped, error) {
	var (
		built   []adapter.Adapter
		skipped []Skipped
	)
	for _, id := range cfg.AdapterIDs() {
		skip := func(reason string) {
			skipped = append(skipped, Skipped{ID: id, Reason: reason})
			log.Debug("backend not available, skipping", "adapter", id, "reason", reason)
		}

		if !adapter.IsRegistered(id) {
			skip("no adapter with this id is compiled into this build")
			continue
		}
		a, err := adapter.Build(id, adapter.Options{
			ID:             id,
			Config:         cfg.Adapters[id],
			ScratchRoot:    cfg.Server.ScratchDir,
			RequestTimeout: cfg.Server.RequestTimeout.Duration(),
			IdleTimeout:    cfg.Server.IdleTimeout.Duration(),
			Logger:         log,
		})
		if err != nil {
			skip(err.Error())
			continue
		}

		probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
		_, err = a.Probe(probeCtx)
		cancel()
		if err != nil {
			skip(ir.AsError(err).Message)
			continue
		}

		log.Info("serving backend", "adapter", id)
		built = append(built, gate.Wrap(a, cfg.Server.MaxConcurrency, cfg.Server.QueueTimeout.Duration()))
	}
	return built, skipped, nil
}

// Router exposes the model router, for `doctor` and tests.
func (s *Server) Router() *router.Router { return s.router }

// Skipped lists adapters that were configured but are not being served.
func (s *Server) Skipped() []Skipped { return s.skipped }

// Handler exposes the fully wired HTTP handler, for tests.
func (s *Server) Handler() http.Handler { return s.handler }

// Addr is the address the gateway binds to.
func (s *Server) Addr() string {
	return net.JoinHostPort(s.cfg.Server.Host, fmt.Sprint(s.cfg.Server.Port))
}

// ListenAndServe runs the gateway until ctx is canceled, then drains in-flight
// requests before returning.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Addr())
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve runs the gateway on an existing listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 30 * time.Second,
		// No WriteTimeout: a request is a whole CLI run, and the per-request
		// and idle deadlines in the runner are what bound it.
		IdleTimeout: 120 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	for _, sk := range s.skipped {
		s.log.Warn("adapter skipped", "adapter", sk.ID, "reason", sk.Reason)
	}
	s.log.Info("agent2api listening",
		"version", version.String(),
		"addr", ln.Addr().String(),
		"models", s.router.ModelIDs(),
		"auth", s.cfg.Server.APIKey != "")

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		grace := s.cfg.Server.ShutdownTimeout.Duration()
		if grace <= 0 {
			grace = 30 * time.Second
		}
		s.log.Info("shutting down; waiting for in-flight requests", "grace", grace.String())
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Give up on graceful and cut the connections.
			_ = srv.Close()
			return err
		}
		return nil
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","models":%d}`+"\n", len(s.router.Entries()))
}
