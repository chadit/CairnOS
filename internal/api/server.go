package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
	"github.com/chadit/CairnOS/internal/memory"
	"github.com/chadit/CairnOS/internal/rules"
	"github.com/chadit/CairnOS/internal/store"
	"github.com/chadit/CairnOS/internal/vault"
	"github.com/chadit/CairnOS/pkg/runner"
)

// readHeaderTimeout bounds slow-header clients on the HTTP listener.
const readHeaderTimeout = 10 * time.Second

// Config wires one Server.
type Config struct {
	// Breakers exposes the engine's circuit breakers to AdminService;
	// the serve wiring passes the engine itself. Nil keeps ResetBreaker
	// answering UNIMPLEMENTED for embedders without an in-process engine.
	Breakers BreakerAdmin

	// GatewayDialCreds is the client credential the HTTP gateway uses to dial
	// the gRPC backend over loopback. It pairs with Creds: when the gRPC
	// listener requires client certs (mTLS), the local gateway needs a
	// matching client identity to reach it. Nil dials insecure, which is
	// correct when Creds is also nil (plaintext dev) and when the gRPC
	// listener accepts plaintext loopback.
	GatewayDialCreds credentials.TransportCredentials

	// Creds is the gRPC transport credential. Non-nil turns on TLS (or mTLS)
	// on the gRPC listener; the A2A mTLS option sets it
	// (.agents/spec/09-a2a-and-hive.md, "Transport security"). Nil keeps the
	// plaintext transport, which the localhost guard still protects.
	Creds credentials.TransportCredentials

	// Store is the durable run store.
	Store store.Store

	// Memory is the memory entry store.
	Memory memory.Store

	// Vault is the document store backing VaultService. Nil leaves the vault
	// methods answering UNIMPLEMENTED, for embedders that do not run a vault.
	Vault vault.Store

	// Rules is the Language Rules Service backing LanguageRulesService. Nil
	// leaves the rules methods answering UNIMPLEMENTED.
	Rules rules.Service

	// Chat is the Phase 9 chat agent backing ChatService. Nil leaves the chat
	// methods answering UNIMPLEMENTED, for nodes that run with chat disabled
	// (.agents/spec/13-roadmap.md, Phase 9).
	Chat ChatAgent

	// Auth turns on Phase 4a token enforcement
	// (.agents/spec/08-auth-and-access.md): a unary and stream
	// interceptor verify the bearer token and its scope on every method
	// except Health and Login. Nil means auth is disabled and every call
	// passes unchecked.
	Auth Authenticator

	// Manager owns run starts, event fan-out, and cancellation.
	Manager *Manager

	// Registry lists the built runners for AdminService.
	Registry *runner.Registry

	// LoginLimiter caps unauthenticated login cost (spec M1): a bounded
	// derivation pool plus a per-peer rate limit on the Login RPC. Nil leaves
	// the login path unbounded, which is the auth-disabled case.
	LoginLimiter *auth.LoginLimiter

	// Logger receives operational events; nil means slog.Default.
	Logger *slog.Logger

	// StreamClock injects the clock the stream recheck reads, so tests drive
	// the cadence deterministically without sleeping. Nil means time.Now.
	StreamClock func() time.Time

	// GRPCAddr and HTTPAddr are the bind addresses. Both pass the Phase 1a
	// localhost guard: loopback or unix only (see normalizeBind).
	GRPCAddr string
	HTTPAddr string

	// WorkflowsDir holds the workflow files RunWorkflow resolves names in.
	WorkflowsDir string

	// Extensions register extra gRPC services, gateway handlers, and HTTP
	// endpoints onto this one server (the A2A adapter, Phase 4b). They are
	// injected as closures rather than imported directly so a service package
	// can depend on this one without a cycle. Nil entries are skipped.
	Extensions []*Extension

	// CORSOrigins is the explicit CORS allowlist for the gateway (spec M1).
	// Empty sends no CORS headers, so the browser enforces same-origin.
	CORSOrigins []string

	// MaxRecvMsgBytes caps a single gRPC message (spec M4). Zero leaves the
	// library default in place.
	MaxRecvMsgBytes int

	// MaxBodyBytes caps a gateway request body (spec M4); an over-limit POST is
	// rejected with 413. Zero leaves the body uncapped.
	MaxBodyBytes int64

	// StreamRecheck is how often an open stream re-verifies its token's
	// expiry and denylist state (spec M4); zero falls back to the default.
	StreamRecheck time.Duration

	// BindPolicy decides which non-loopback binds the guard accepts; the
	// zero value keeps the loopback-only behavior.
	BindPolicy BindPolicy
}

// Extension bundles one optional service's registration hooks plus any extra
// HTTP routes it serves outside the gateway (the A2A well-known card endpoint
// is one). The serve wiring builds these where both the api and the service
// package are in scope, so this package never imports the service.
type Extension struct {
	// RegisterGRPC registers the extension's gRPC service on the server.
	RegisterGRPC func(*grpc.Server)

	// RegisterGateway registers the extension's gateway handler so its methods
	// also answer over HTTP/JSON. Nil means the extension is gRPC-only.
	RegisterGateway func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error

	// HTTPRoutes are extra handlers mounted on the root HTTP mux, outside the
	// gateway and outside the auth interceptor (public discovery endpoints).
	HTTPRoutes map[string]http.HandlerFunc

	// Name labels the extension in logs.
	Name string
}

// Server is the Phase 1a API process: one gRPC server (with reflection) and
// one HTTP listener carrying the generated gateway plus /openapi.json and
// /routes.
type Server struct {
	cfg    *Config
	logger *slog.Logger

	grpcServer *grpc.Server
	httpServer *http.Server
	gwConn     *grpc.ClientConn
	grpcLis    net.Listener
	httpLis    net.Listener
}

// NewServer builds an unstarted server.
func NewServer(cfg *Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Server{cfg: cfg, logger: logger}
}

// Start binds both listeners (refusing non-loopback addresses), registers
// the services and reflection, mounts the gateway, and serves in background
// goroutines. It returns once both listeners accept, so callers can read
// the bound addresses immediately; Shutdown stops everything.
func (s *Server) Start(ctx context.Context) error {
	if err := s.listen(ctx); err != nil {
		return err
	}

	s.startGRPC()

	if err := s.startGateway(ctx); err != nil {
		s.grpcServer.Stop()
		s.closeListeners()

		return err
	}

	return nil
}

// GRPCAddr reports the bound gRPC address, useful when the config asked for
// an ephemeral port.
func (s *Server) GRPCAddr() string {
	if s.grpcLis == nil {
		return ""
	}

	return s.grpcLis.Addr().String()
}

// HTTPAddr reports the bound HTTP address.
func (s *Server) HTTPAddr() string {
	if s.httpLis == nil {
		return ""
	}

	return s.httpLis.Addr().String()
}

// Shutdown stops the server: the HTTP listener drains, active runs are
// canceled so their streams finish, and the gRPC server gets a graceful stop
// bounded by ctx, falling back to a hard stop when the deadline passes.
func (s *Server) Shutdown(ctx context.Context) error {
	var errs []error

	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown http server: %w", err))
		}
	}

	if s.cfg.Manager != nil {
		s.cfg.Manager.Shutdown()
	}

	if s.grpcServer != nil {
		s.gracefulStopGRPC(ctx)
	}

	if s.gwConn != nil {
		if err := s.gwConn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close gateway connection: %w", err))
		}
	}

	if joined := errors.Join(errs...); joined != nil {
		return fmt.Errorf("shutdown api server: %w", joined)
	}

	return nil
}

// listen binds both addresses after the network guard passes, logging the
// loud warning when a deliberately open bind was allowed.
func (s *Server) listen(ctx context.Context) error {
	grpcNet, grpcAddr, grpcWarn, err := normalizeBind(s.cfg.GRPCAddr, s.cfg.BindPolicy)
	if err != nil {
		return fmt.Errorf("grpc bind: %w", err)
	}

	httpNet, httpAddr, httpWarn, err := normalizeBind(s.cfg.HTTPAddr, s.cfg.BindPolicy)
	if err != nil {
		return fmt.Errorf("http bind: %w", err)
	}

	for _, warn := range []string{grpcWarn, httpWarn} {
		if warn != "" {
			s.logger.Warn(warn)
		}
	}

	listenCfg := &net.ListenConfig{}

	s.grpcLis, err = listenCfg.Listen(ctx, grpcNet, grpcAddr)
	if err != nil {
		return fmt.Errorf("listen %s %s: %w", grpcNet, grpcAddr, err)
	}

	s.httpLis, err = listenCfg.Listen(ctx, httpNet, httpAddr)
	if err != nil {
		s.closeListeners()

		return fmt.Errorf("listen %s %s: %w", httpNet, httpAddr, err)
	}

	return nil
}

// startGRPC registers the services plus reflection and serves in the
// background. Reflection is the discovery surface: grpcurl and buf list
// every service and message without a hand-rolled registry; with auth
// enabled, reflection sits behind the interceptor's fail-closed default
// like every unlisted method, so discovery needs an admin-scoped token.
func (s *Server) startGRPC() {
	var options []grpc.ServerOption

	// Cap one received message explicitly (spec M4) so an over-limit frame is
	// rejected by the transport rather than relying on the library default.
	if s.cfg.MaxRecvMsgBytes > 0 {
		options = append(options, grpc.MaxRecvMsgSize(s.cfg.MaxRecvMsgBytes))
	}

	if s.cfg.Auth != nil {
		guard := newAuthGuard(s.cfg.Auth, s.cfg.StreamRecheck, s.cfg.StreamClock)
		options = append(options,
			grpc.ChainUnaryInterceptor(guard.unary),
			grpc.ChainStreamInterceptor(guard.stream))
	}

	// mTLS proves the connection; the token proves the caller's scopes. The
	// two layer independently, so the interceptor above still runs on a TLS
	// connection (.agents/spec/09-a2a-and-hive.md, "Transport security").
	if s.cfg.Creds != nil {
		options = append(options, grpc.Creds(s.cfg.Creds))
	}

	s.grpcServer = grpc.NewServer(options...)

	cairnosv1.RegisterLoginServiceServer(s.grpcServer, &loginService{
		authenticator: s.cfg.Auth,
		limiter:       s.cfg.LoginLimiter,
		logger:        s.logger,
	})
	cairnosv1.RegisterWorkflowServiceServer(s.grpcServer, &workflowService{
		store:         s.cfg.Store,
		manager:       s.cfg.Manager,
		workflowsDir:  s.cfg.WorkflowsDir,
		authenticator: s.cfg.Auth,
		logger:        s.logger,
	})
	cairnosv1.RegisterMemoryServiceServer(s.grpcServer, &memoryService{store: s.cfg.Memory})

	if s.cfg.Vault != nil {
		cairnosv1.RegisterVaultServiceServer(s.grpcServer, &vaultService{store: s.cfg.Vault})
	}

	if s.cfg.Rules != nil {
		cairnosv1.RegisterLanguageRulesServiceServer(s.grpcServer, &languageRulesService{store: s.cfg.Rules})
	}

	if s.cfg.Chat != nil {
		cairnosv1.RegisterChatServiceServer(s.grpcServer, &chatService{agent: s.cfg.Chat})
	}

	cairnosv1.RegisterSuggestionServiceServer(s.grpcServer, &suggestionService{store: s.cfg.Store})
	cairnosv1.RegisterAdminServiceServer(s.grpcServer, &adminService{
		store:    s.cfg.Store,
		registry: s.cfg.Registry,
		breakers: s.cfg.Breakers,
	})
	cairnosv1.RegisterGovernanceServiceServer(s.grpcServer, &governanceService{
		store:         s.cfg.Store,
		manager:       s.cfg.Manager,
		resolver:      NewWorkflowResolver(s.cfg.WorkflowsDir, s.logger),
		authenticator: s.cfg.Auth,
		logger:        s.logger,
	})

	for _, ext := range s.cfg.Extensions {
		if ext != nil && ext.RegisterGRPC != nil {
			ext.RegisterGRPC(s.grpcServer)
		}
	}

	reflection.Register(s.grpcServer)

	go func() {
		if err := s.grpcServer.Serve(s.grpcLis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			s.logger.Error("grpc server stopped", "error", err)
		}
	}()
}

// startGateway dials the gRPC listener over loopback, registers every
// generated gateway handler on one mux, mounts the discovery endpoints, and
// serves HTTP in the background. Dialing (rather than the in-process
// handler) keeps server streaming working over the gateway.
func (s *Server) startGateway(ctx context.Context) error {
	conn, err := grpc.NewClient(s.gatewayTarget(), grpc.WithTransportCredentials(s.gatewayDialCreds()))
	if err != nil {
		return fmt.Errorf("dial gateway backend: %w", err)
	}

	s.gwConn = conn

	mux := newGatewayMux()

	registrations := []func(context.Context, *runtime.ServeMux, *grpc.ClientConn) error{
		cairnosv1.RegisterLoginServiceHandler,
		cairnosv1.RegisterWorkflowServiceHandler,
		cairnosv1.RegisterMemoryServiceHandler,
		cairnosv1.RegisterSuggestionServiceHandler,
		cairnosv1.RegisterAdminServiceHandler,
		cairnosv1.RegisterGovernanceServiceHandler,
	}

	if s.cfg.Vault != nil {
		registrations = append(registrations, cairnosv1.RegisterVaultServiceHandler)
	}

	if s.cfg.Rules != nil {
		registrations = append(registrations, cairnosv1.RegisterLanguageRulesServiceHandler)
	}

	if s.cfg.Chat != nil {
		registrations = append(registrations, cairnosv1.RegisterChatServiceHandler)
	}

	for _, ext := range s.cfg.Extensions {
		if ext != nil && ext.RegisterGateway != nil {
			registrations = append(registrations, ext.RegisterGateway)
		}
	}

	for _, register := range registrations {
		if registerErr := register(ctx, mux, conn); registerErr != nil {
			return fmt.Errorf("register gateway handler: %w", registerErr)
		}
	}

	routes, err := buildRoutes()
	if err != nil {
		return err
	}

	// Deliberate inconsistency (spec L4): /openapi.json and /routes stay public
	// so tooling can discover them, while gRPC server reflection sits behind
	// the auth interceptor's fail-closed default (admin-scoped). The HTTP
	// discovery docs expose only method shapes, never data, so gating them is
	// left as a future operator option rather than a change made here.
	root := http.NewServeMux()
	root.Handle("/", limitBody(s.cfg.MaxBodyBytes, mux))
	root.HandleFunc("GET /openapi.json", openapiHandler())
	root.HandleFunc("GET /routes", routesHandler(routes))
	s.mountExtensionRoutes(root)

	// Security headers and the CORS allowlist wrap the whole mux, so every
	// response (gateway, discovery, and extension routes) carries the hardening
	// headers and only allowlisted origins get CORS (spec M1).
	handler := securityHeaders(corsAllowlist(s.cfg.CORSOrigins, root))

	s.httpServer = &http.Server{Handler: handler, ReadHeaderTimeout: readHeaderTimeout}

	go func() {
		if err := s.httpServer.Serve(s.httpLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("http server stopped", "error", err)
		}
	}()

	return nil
}

// gatewayTarget renders the bound gRPC address as a grpc.NewClient target.
func (s *Server) gatewayTarget() string {
	if s.grpcLis.Addr().Network() == networkUnix {
		return unixPrefix + s.grpcLis.Addr().String()
	}

	return s.grpcLis.Addr().String()
}

// gatewayDialCreds picks the credential the gateway dials the backend with:
// the configured client credential when one is set (mTLS), or insecure for the
// plaintext loopback default.
func (s *Server) gatewayDialCreds() credentials.TransportCredentials {
	if s.cfg.GatewayDialCreds != nil {
		return s.cfg.GatewayDialCreds
	}

	return insecure.NewCredentials()
}

// mountExtensionRoutes mounts each extension's extra HTTP handlers on the root
// mux, outside the gateway and the auth interceptor. The A2A well-known card
// endpoint registers here: discovery is public, and the card's own signature
// is the trust mechanism.
func (s *Server) mountExtensionRoutes(root *http.ServeMux) {
	for _, ext := range s.cfg.Extensions {
		if ext == nil {
			continue
		}

		for pattern, handler := range ext.HTTPRoutes {
			root.HandleFunc(pattern, handler)
		}
	}
}

// gracefulStopGRPC drains in-flight RPCs, hard-stopping when ctx ends first.
func (s *Server) gracefulStopGRPC(ctx context.Context) {
	done := make(chan struct{})

	go func() {
		s.grpcServer.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		s.logger.Warn("graceful stop timed out; forcing grpc stop")
		s.grpcServer.Stop()
		<-done
	}
}

// closeListeners releases any bound listeners after a partial Start.
func (s *Server) closeListeners() {
	for _, lis := range []net.Listener{s.grpcLis, s.httpLis} {
		if lis == nil {
			continue
		}

		// Listener close failures during teardown only matter as noise; the
		// process is abandoning the sockets either way.
		if err := lis.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logger.Warn("close listener", "error", err)
		}
	}
}
