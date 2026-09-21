package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cairnosv1 "github.com/chadit/CairnOS/internal/api/gen/cairnos/v1"
)

// Shared fixtures for the vault and rules API tests; goconst applies in tests.
const (
	vaultTitle = "design note"
	ruleLangGo = "go"
	ruleKeyMax = "max_line"
)

func TestVaultCRUDAndQueryOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	ctx := t.Context()

	put, err := fix.vault.PutDocument(ctx, &cairnosv1.PutDocumentRequest{
		Document: &cairnosv1.VaultDocument{
			Title: vaultTitle,
			Body:  "kafka lag postmortem",
			Tags:  []string{"infra", "project:finance-app"},
		},
	})
	if err != nil {
		t.Fatalf("PutDocument: %v", err)
	}

	got, err := fix.vault.GetDocument(ctx, &cairnosv1.GetDocumentRequest{Id: put.GetId()})
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}

	if got.GetDocument().GetTitle() != vaultTitle {
		t.Fatalf("title = %q, want %q", got.GetDocument().GetTitle(), vaultTitle)
	}

	// A tag-and-project query finds the document.
	queried, err := fix.vault.QueryVault(ctx, &cairnosv1.QueryVaultRequest{
		Tags:    []string{"infra"},
		Project: "finance-app",
		Text:    "kafka",
	})
	if err != nil {
		t.Fatalf("QueryVault: %v", err)
	}

	if len(queried.GetDocuments()) != 1 || queried.GetDocuments()[0].GetId() != put.GetId() {
		t.Fatalf("query returned %d docs, want the put document", len(queried.GetDocuments()))
	}

	if _, deleteDocumentErr := fix.vault.DeleteDocument(ctx, &cairnosv1.DeleteDocumentRequest{Id: put.GetId()}); deleteDocumentErr != nil {
		t.Fatalf("DeleteDocument: %v", deleteDocumentErr)
	}

	_, err = fix.vault.GetDocument(ctx, &cairnosv1.GetDocumentRequest{Id: put.GetId()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetDocument after delete code = %v, want NotFound", status.Code(err))
	}
}

func TestVaultBacklinkOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	ctx := t.Context()

	leaf, err := fix.vault.PutDocument(ctx, &cairnosv1.PutDocumentRequest{
		Document: &cairnosv1.VaultDocument{Title: "leaf", Body: "leaf body"},
	})
	if err != nil {
		t.Fatalf("PutDocument leaf: %v", err)
	}

	hub, err := fix.vault.PutDocument(ctx, &cairnosv1.PutDocumentRequest{
		Document: &cairnosv1.VaultDocument{Title: "hub", Body: "hub body", Links: []string{leaf.GetId()}},
	})
	if err != nil {
		t.Fatalf("PutDocument hub: %v", err)
	}

	got, err := fix.vault.GetDocument(ctx, &cairnosv1.GetDocumentRequest{Id: leaf.GetId()})
	if err != nil {
		t.Fatalf("GetDocument leaf: %v", err)
	}

	backlinks := got.GetDocument().GetBacklinks()
	if len(backlinks) != 1 || backlinks[0] != hub.GetId() {
		t.Fatalf("leaf backlinks = %v, want [%s]", backlinks, hub.GetId())
	}
}

func TestRulesMergeAndListOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startServer(t)
	ctx := t.Context()

	putRule := func(project, key, value string) {
		t.Helper()

		_, err := fix.rules.PutRule(ctx, &cairnosv1.PutRuleRequest{
			Rule: &cairnosv1.LanguageRule{Language: ruleLangGo, Project: project, Key: key, Value: value},
		})
		if err != nil {
			t.Fatalf("PutRule %s/%s: %v", project, key, err)
		}
	}

	putRule("", ruleKeyMax, "120")
	putRule("", "errors", "wrap")
	putRule("finance-app", ruleKeyMax, "100")

	merged, err := fix.rules.GetRules(ctx, &cairnosv1.GetRulesRequest{Language: ruleLangGo, Project: "finance-app"})
	if err != nil {
		t.Fatalf("GetRules: %v", err)
	}

	values := make(map[string]string, len(merged.GetRules()))
	for _, rule := range merged.GetRules() {
		values[rule.GetKey()] = rule.GetValue()
	}

	if values[ruleKeyMax] != "100" || values["errors"] != "wrap" {
		t.Fatalf("merged rules = %v, want project override on max_line and surviving default errors", values)
	}

	langs, err := fix.rules.ListLanguages(ctx, &cairnosv1.ListLanguagesRequest{})
	if err != nil {
		t.Fatalf("ListLanguages: %v", err)
	}

	if len(langs.GetLanguages()) != 1 || langs.GetLanguages()[0] != ruleLangGo {
		t.Fatalf("languages = %v, want [go]", langs.GetLanguages())
	}
}

func TestRulesUnknownLanguageOverGRPC(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	_, err := fix.rules.GetRules(t.Context(), &cairnosv1.GetRulesRequest{Language: "fortran"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("GetRules unknown language code = %v, want NotFound", status.Code(err))
	}
}

func TestVaultPutAndQueryOverGateway(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	resp := httpPost(t, fix.transport, fix.httpBase+"/v1/vault", `{"document":{"title":"gateway note","body":"http body","tags":["gw"]}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /v1/vault status = %d, want 200", resp.StatusCode)
	}

	var put struct {
		ID string `json:"id"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&put); err != nil {
		t.Fatalf("decode put response: %v", err)
	}

	if put.ID == "" {
		t.Fatal("gateway PutDocument returned no id")
	}

	query := httpPost(t, fix.transport, fix.httpBase+"/v1/vault/query", `{"text":"http"}`)
	defer func() { _ = query.Body.Close() }()

	if query.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/vault/query status = %d, want 200", query.StatusCode)
	}

	var result struct {
		Documents []struct {
			ID string `json:"id"`
		} `json:"documents"`
	}

	if err := json.NewDecoder(query.Body).Decode(&result); err != nil {
		t.Fatalf("decode query response: %v", err)
	}

	if len(result.Documents) != 1 || result.Documents[0].ID != put.ID {
		t.Fatalf("gateway query returned %d docs, want the put document", len(result.Documents))
	}
}

func TestRulesGetOverGateway(t *testing.T) {
	t.Parallel()

	fix := startServer(t)

	put := httpPost(t, fix.transport, fix.httpBase+"/v1/rules", `{"rule":{"language":"go","key":"indent","value":"tabs"}}`)
	defer func() { _ = put.Body.Close() }()

	if put.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/rules status = %d, want 200", put.StatusCode)
	}

	code, body := httpGet(t, fix.transport, fix.httpBase+"/v1/rules/go")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/rules/go = %d (%s), want 200", code, body)
	}

	var got struct {
		Language string `json:"language"`
		Rules    []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"rules"`
	}

	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse rules body %q: %v", body, err)
	}

	if got.Language != ruleLangGo || len(got.Rules) != 1 || got.Rules[0].Value != "tabs" {
		t.Fatalf("gateway rules = %+v, want one go/indent=tabs rule", got)
	}
}
