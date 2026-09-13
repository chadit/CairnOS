package api_test

import (
	"testing"

	"google.golang.org/grpc"

	"github.com/chadit/CairnOS/internal/api"
	a2av1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/a2a/v1"
	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
	"github.com/chadit/CairnOS/internal/auth"
)

// scopePublic marks a method that carries no token at all, which is a different
// answer from any scope and so needs a label of its own in the tables below.
const scopePublic = "<public>"

// serviceScopes is one served service and the scopes its methods take: want is
// the scope the whole service shares, and byMethod names the exceptions. Every
// service the API server registers has a row, and TestEveryServedServiceHasARow
// is what keeps a new one from being added without one.
type serviceScopes struct {
	desc     *grpc.ServiceDesc
	byMethod map[string]string
	name     string
	want     string
}

// servedServices is the whole scope table restated from the outside. A method
// that falls through ScopeForMethod's default answers admin, so a forgotten
// entry compiles and serves, surfacing only when an operator's correctly scoped
// key is refused.
func servedServices() []serviceScopes {
	return []serviceScopes{
		{
			name: "AdminService", desc: &cairnosv1.AdminService_ServiceDesc, want: auth.ScopeAdmin,
			byMethod: map[string]string{"Health": scopePublic, "ListRunners": auth.ScopeReadWorkflow},
		},
		{
			name: "LoginService", desc: &cairnosv1.LoginService_ServiceDesc, want: auth.ScopeAdmin,
			byMethod: map[string]string{"Login": scopePublic},
		},
		{
			name: "WorkflowService", desc: &cairnosv1.WorkflowService_ServiceDesc, want: auth.ScopeWriteWorkflow,
			byMethod: map[string]string{
				"ValidateWorkflow": auth.ScopeReadWorkflow,
				"GetRun":           auth.ScopeReadRun,
				"ListRuns":         auth.ScopeReadRun,
			},
		},
		{
			name: "MemoryService", desc: &cairnosv1.MemoryService_ServiceDesc, want: auth.ScopeWriteMemory,
			byMethod: map[string]string{"GetMemory": auth.ScopeReadMemory, "SearchMemory": auth.ScopeReadMemory},
		},
		{
			name: "VaultService", desc: &cairnosv1.VaultService_ServiceDesc, want: auth.ScopeWriteVault,
			byMethod: map[string]string{"GetDocument": auth.ScopeReadVault, "QueryVault": auth.ScopeReadVault},
		},
		{
			name: "LanguageRulesService", desc: &cairnosv1.LanguageRulesService_ServiceDesc, want: auth.ScopeReadVault,
			byMethod: map[string]string{"PutRule": auth.ScopeWriteVault},
		},
		{
			name: "SuggestionService", desc: &cairnosv1.SuggestionService_ServiceDesc, want: auth.ScopeAdmin,
			byMethod: map[string]string{"ListSuggestions": auth.ScopeReadRun},
		},
		{name: "ChatService", desc: &cairnosv1.ChatService_ServiceDesc, want: auth.ScopeChatSession},
		{name: "GovernanceService", desc: &cairnosv1.GovernanceService_ServiceDesc, want: auth.ScopeAdmin},
		{name: "MessagingService", desc: &cairnosv1.MessagingService_ServiceDesc, want: auth.ScopeMessageSend},
		{name: "A2AService", desc: &a2av1.A2AService_ServiceDesc, want: auth.ScopeA2ATask},
	}
}

// TestServiceScopeTable walks every method of every served service and pins the
// scope it takes, unary and streaming alike.
func TestServiceScopeTable(t *testing.T) {
	t.Parallel()

	for _, service := range servedServices() {
		t.Run(service.name, func(t *testing.T) {
			t.Parallel()

			methods := fullMethodNames(service.desc)
			if len(methods) == 0 {
				t.Fatalf("%s descriptor lists no methods", service.name)
			}

			for index, method := range methods {
				assertScope(t, method, service.wantFor(index))
			}
		})
	}
}

// wantFor returns the scope the method at index takes: the service's shared
// answer unless byMethod names an exception for it.
func (s serviceScopes) wantFor(index int) string {
	if scope, named := s.byMethod[methodName(s.desc, index)]; named {
		return scope
	}

	return s.want
}

// TestEveryServedServiceHasARow proves the table above covers every generated
// service the API server registers. Without it a new service falls through to
// admin, an operator's correctly scoped key is refused, and no test fires.
func TestEveryServedServiceHasARow(t *testing.T) {
	t.Parallel()

	covered := make(map[string]struct{}, len(servedServices()))
	for _, service := range servedServices() {
		covered[service.desc.ServiceName] = struct{}{}
	}

	for _, name := range registeredServiceNames() {
		if _, listed := covered[name]; !listed {
			t.Errorf("service %q is registered on the API server but has no row in servedServices", name)
		}
	}
}

// registeredServiceNames lists the generated services the API server serves.
// cairnos.plugin.v1.RunnerPlugin is deliberately absent: a runner plugin is the
// client of that service, and the API server never registers it, so the scope
// table has nothing to say about its methods.
func registeredServiceNames() []string {
	return []string{
		cairnosv1.AdminService_ServiceDesc.ServiceName,
		cairnosv1.LoginService_ServiceDesc.ServiceName,
		cairnosv1.WorkflowService_ServiceDesc.ServiceName,
		cairnosv1.MemoryService_ServiceDesc.ServiceName,
		cairnosv1.VaultService_ServiceDesc.ServiceName,
		cairnosv1.LanguageRulesService_ServiceDesc.ServiceName,
		cairnosv1.SuggestionService_ServiceDesc.ServiceName,
		cairnosv1.ChatService_ServiceDesc.ServiceName,
		cairnosv1.GovernanceService_ServiceDesc.ServiceName,
		cairnosv1.MessagingService_ServiceDesc.ServiceName,
		a2av1.A2AService_ServiceDesc.ServiceName,
	}
}

// assertScope checks one full method string against the scope it should take,
// with scopePublic standing for a method that carries no token.
func assertScope(t *testing.T, fullMethod, want string) {
	t.Helper()

	scope, public := api.ScopeForMethod(fullMethod)

	if want == scopePublic {
		if !public {
			t.Errorf("api.ScopeForMethod(%q) = %q, want a public method", fullMethod, scope)
		}

		return
	}

	if public {
		t.Errorf("api.ScopeForMethod(%q) = public, want scope %q", fullMethod, want)

		return
	}

	if scope != want {
		t.Errorf("api.ScopeForMethod(%q) = %q, want %q", fullMethod, scope, want)
	}
}

// fullMethodNames renders a descriptor's unary and streaming methods as the
// "/package.Service/Method" strings the interceptor switches on.
func fullMethodNames(desc *grpc.ServiceDesc) []string {
	names := make([]string, 0, len(desc.Methods)+len(desc.Streams))

	for _, method := range desc.Methods {
		names = append(names, "/"+desc.ServiceName+"/"+method.MethodName)
	}

	for _, stream := range desc.Streams {
		names = append(names, "/"+desc.ServiceName+"/"+stream.StreamName)
	}

	return names
}

// methodName returns the bare method name at the same index fullMethodNames
// used, so the exception map is keyed by the short name a reader recognizes.
func methodName(desc *grpc.ServiceDesc, index int) string {
	if index < len(desc.Methods) {
		return desc.Methods[index].MethodName
	}

	return desc.Streams[index-len(desc.Methods)].StreamName
}
