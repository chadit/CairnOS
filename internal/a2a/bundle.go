package a2a

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/chadit/CairnOS/internal/api"
	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	"github.com/chadit/CairnOS/internal/store"
)

// defaultSkills is the capability list the card advertises: two skills. The
// node runs any workflow by name through
// CreateTask, so run_workflow is the headline skill; query_memory mirrors the
// memory surface the same token reaches.
func defaultSkills() []Skill {
	return []Skill{
		{Name: "run_workflow", Description: "Run a named workflow with inputs."},
		{Name: "query_memory", Description: "Search memory and the vault."},
	}
}

// BundleConfig carries everything the A2A bundle needs at boot. The wiring
// fills it where both the api and a2a packages are in scope, so neither has to
// import the other's construction details.
type BundleConfig struct {
	// Bridge is the chat core SendMessage runs a conversational turn through.
	// Nil means chat is off on this node, and SendMessage answers
	// FailedPrecondition rather than pretending a turn ran.
	Bridge BridgeHandler

	// Store, Resolver, and Manager back the A2A service handlers.
	Store store.Store

	// Logger receives the well-known handler's write failures.
	Logger *slog.Logger

	// Resolver resolves workflow references for the service handlers.
	Resolver *api.WorkflowResolver

	// Manager runs the workflows the service handlers start.
	Manager *api.Manager

	// TLS is the mTLS transport config; the zero value means plaintext.
	TLS TLSConfig

	// LoginURL is the card's auth login endpoint, where a consumer exchanges
	// an access key for a bearer token.
	LoginURL string

	// NodeName is the node's name on its card.
	NodeName string

	// Endpoint is the address the card advertises (e.g. "grpc://host:7777").
	Endpoint string

	// Version is the build the card advertises for this node.
	Version string

	// Channels are the chat surfaces the card advertises, one per enabled chat
	// identity. Nil leaves the card's channels key off entirely, which is
	// what a node with chat disabled publishes.
	Channels []Channel

	// Skills overrides the advertised capability list; nil uses the default.
	Skills []Skill

	// SigningKey signs the card. ResolveSigningKey produces it.
	SigningKey ed25519.PrivateKey
}

// Bundle is the assembled A2A surface: the signed card, the server credentials
// when mTLS is on, and the api.Extension that registers the service, its
// gateway handler, and the well-known endpoint onto the shared API server.
type Bundle struct {
	// Creds is the gRPC server credential when mTLS is configured, else nil.
	Creds credentials.TransportCredentials

	// Card is the signed agent card, kept so the CLI can print it.
	Card *Card

	// Extension plugs the service, gateway handler, and card route into the
	// API server.
	Extension *api.Extension

	// tlsCfg is retained so Attach can build the matching client credential
	// the local gateway dials the mTLS listener with.
	tlsCfg TLSConfig

	// CardJSON is the marshaled signed card the well-known handler serves.
	CardJSON []byte
}

// NewBundle assembles and signs the card, builds the mTLS credentials when
// configured, and returns the Extension that wires the A2A service into the
// shared API server. A failure here (bad signing key, broken mTLS files)
// surfaces at boot so a misconfigured node never serves an A2A surface it
// cannot back.
func NewBundle(cfg *BundleConfig) (*Bundle, error) {
	skills := cfg.Skills
	if skills == nil {
		skills = defaultSkills()
	}

	card := &Card{
		Name:     cfg.NodeName,
		Version:  cfg.Version,
		Protocol: ProtocolVersion,
		Endpoint: cfg.Endpoint,
		Skills:   skills,
		Channels: cfg.Channels,
		Auth:     AuthInfo{Type: AuthTypeJWT, Login: cfg.LoginURL},
	}

	if err := card.Sign(cfg.SigningKey); err != nil {
		return nil, err
	}

	cardJSON, err := card.Marshal()
	if err != nil {
		return nil, err
	}

	// A nil credential is the plaintext default when no mTLS block is set.
	var creds credentials.TransportCredentials

	if cfg.TLS.Configured() {
		serverTLS, err := cfg.TLS.ServerTLSConfig()
		if err != nil {
			return nil, err
		}

		creds = credentials.NewTLS(serverTLS)
	}

	service := NewService(cfg.Manager, cfg.Resolver, cfg.Store, cfg.Bridge)

	extension := &api.Extension{
		Name: "a2a",
		RegisterGRPC: func(server *grpc.Server) {
			a2av1.RegisterA2AServiceServer(server, service)
		},
		RegisterGateway: registerGateway,
		HTTPRoutes: map[string]http.HandlerFunc{
			"GET " + WellKnownPath: CardHandler(cardJSON, cfg.Logger),
		},
	}

	return &Bundle{Card: card, CardJSON: cardJSON, Creds: creds, Extension: extension, tlsCfg: cfg.TLS}, nil
}

// Attach wires the bundle's extension and credentials into an api.Config. When
// mTLS is on, it also builds the client credential the HTTP gateway dials the
// listener with, so the local gateway hop satisfies RequireAndVerifyClientCert.
// A failure building that client credential returns an error rather than
// silently leaving the gateway unable to reach its own backend.
func (b *Bundle) Attach(cfg *api.Config) error {
	cfg.Extensions = append(cfg.Extensions, b.Extension)

	if b.Creds == nil {
		return nil
	}

	cfg.Creds = b.Creds

	clientTLS, err := b.tlsCfg.ClientTLSConfig()
	if err != nil {
		return err
	}

	cfg.GatewayDialCreds = credentials.NewTLS(clientTLS)

	return nil
}

// registerGateway adapts the generated A2A gateway registrar to the
// Extension hook, wrapping its error so the gateway wiring reports which
// handler failed.
//
//nolint:wrapcheck // the generated registrar returns a plain error; the api server wraps it with the handler context
func registerGateway(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
	return a2av1.RegisterA2AServiceHandler(ctx, mux, conn)
}
