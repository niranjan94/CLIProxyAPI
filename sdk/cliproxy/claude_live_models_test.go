package cliproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const (
	claudeLiveMythosList = `{"data":[
		{"type":"model","id":"claude-mythos-5-1","display_name":"Claude Mythos 5.1","created_at":"2026-09-01T00:00:00Z"},
		{"type":"model","id":"claude-fable-5-1","display_name":"Claude Fable 5.1","created_at":"2026-09-01T00:00:00Z"}
	],"has_more":false}`
	claudeLivePlainList = `{"data":[
		{"type":"model","id":"claude-fable-5-1","display_name":"Claude Fable 5.1","created_at":"2026-09-01T00:00:00Z"}
	],"has_more":false}`
)

type claudeLiveFakeUpstream struct {
	mu        sync.Mutex
	responses map[string]func() (*http.Response, error)
	requests  []*http.Request
}

func (f *claudeLiveFakeUpstream) do(_ context.Context, _ *coreauth.Manager, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	respond := f.responses[auth.ID]
	f.mu.Unlock()
	if respond == nil {
		return nil, errors.New("no response configured")
	}
	return respond()
}

func (f *claudeLiveFakeUpstream) set(authID string, respond func() (*http.Response, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[authID] = respond
}

func claudeLiveJSON(body string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
}

func claudeLiveStatus(code int) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	}
}

func installClaudeLiveFake(t *testing.T) *claudeLiveFakeUpstream {
	t.Helper()
	fake := &claudeLiveFakeUpstream{responses: make(map[string]func() (*http.Response, error))}
	previous := claudeLiveModelsDo
	claudeLiveModelsDo = fake.do
	t.Cleanup(func() { claudeLiveModelsDo = previous })
	return fake
}

func newClaudeLiveOAuthAuth(id, email string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:         id,
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"auth_kind": "oauth"},
		Metadata:   map[string]any{"access_token": "token-" + id, "email": email},
	}
}

func newClaudeLiveService(t *testing.T, liveModels bool, auths ...*coreauth.Auth) *Service {
	t.Helper()
	cfg := &config.Config{}
	cfg.Claude.LiveModels = liveModels
	service := &Service{cfg: cfg, coreManager: coreauth.NewManager(nil, nil, nil)}
	reg := GlobalModelRegistry()
	for _, auth := range auths {
		if _, errRegister := service.coreManager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register auth %s: %v", auth.ID, errRegister)
		}
		authID := auth.ID
		reg.UnregisterClient(authID)
		t.Cleanup(func() { reg.UnregisterClient(authID) })
		service.registerModelsForAuth(context.Background(), auth)
	}
	return service
}

func runClaudeLiveSync(service *Service, state *claudeLiveModelSyncState, now time.Time) {
	service.syncClaudeLiveModels(context.Background(), state, now)
	state.reads.Wait()
}

func TestParseClaudeLiveModels(t *testing.T) {
	models, errParse := parseClaudeLiveModels([]byte(`{"data":[
		{"id":"claude-mythos-5-1","display_name":"Claude Mythos 5.1","created_at":"2026-09-01T00:00:00Z","max_input_tokens":1000000,"max_tokens":128000},
		{"id":"not-claude"},
		{"id":""}
	]}`))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if len(models) != 1 {
		t.Fatalf("models = %+v, want only claude-mythos-5-1", models)
	}
	got := models[0]
	if got.id != "claude-mythos-5-1" || got.displayName != "Claude Mythos 5.1" || got.contextLength != 1000000 || got.maxCompletionTokens != 128000 {
		t.Fatalf("model = %+v", got)
	}
	if got.created != time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("created = %d", got.created)
	}

	for _, body := range []string{`{}`, `{"data":null}`, `{"error":{"type":"permission_error"}}`, `not json`} {
		if _, errMissing := parseClaudeLiveModels([]byte(body)); errMissing == nil {
			t.Fatalf("parse(%s) succeeded, want error for missing model list", body)
		}
	}
}

func TestClaudeLiveModelInfo_MythosInheritsFableOfSameVersion(t *testing.T) {
	catalog := internalregistry.GetClaudeModels()
	var fable *ModelInfo
	for _, model := range catalog {
		if model.ID == "claude-fable-5-1" {
			fable = model
		}
	}
	if fable == nil {
		t.Skip("bundled catalog has no claude-fable-5-1")
	}

	info := claudeLiveModelInfo(claudeLiveModel{id: "claude-mythos-5-1", displayName: "Claude Mythos 5.1", created: 42}, catalog)
	if info.ID != "claude-mythos-5-1" || info.DisplayName != "Claude Mythos 5.1" || info.Created != 42 {
		t.Fatalf("identity not taken from the model list: %+v", info)
	}
	if info.ContextLength != fable.ContextLength || info.MaxCompletionTokens != fable.MaxCompletionTokens {
		t.Fatalf("limits = %d/%d, want Fable 5.1's %d/%d", info.ContextLength, info.MaxCompletionTokens, fable.ContextLength, fable.MaxCompletionTokens)
	}
	if info.Thinking == nil || fable.Thinking == nil || strings.Join(info.Thinking.Levels, ",") != strings.Join(fable.Thinking.Levels, ",") {
		t.Fatalf("thinking = %+v, want Fable 5.1's %+v", info.Thinking, fable.Thinking)
	}
	if info.Type != "claude" || info.OwnedBy != fable.OwnedBy {
		t.Fatalf("type/owner = %s/%s", info.Type, info.OwnedBy)
	}
	if info.Thinking == fable.Thinking {
		t.Fatal("live model shares the catalog model's thinking definition")
	}

	reported := claudeLiveModelInfo(claudeLiveModel{id: "claude-mythos-5-1", contextLength: 500000, maxCompletionTokens: 64000}, catalog)
	if reported.ContextLength != 500000 || reported.MaxCompletionTokens != 64000 || reported.DisplayName != "claude-mythos-5-1" {
		t.Fatalf("reported limits not applied: %+v", reported)
	}
}

func TestClaudeLiveModelTemplate_FallsBackToClosestName(t *testing.T) {
	catalog := []*ModelInfo{
		{ID: "claude-opus-5", Created: 1},
		{ID: "claude-opus-5-5", Created: 3},
		{ID: "claude-sonnet-5-5", Created: 4},
	}
	if got := claudeLiveModelTemplate("claude-opus-5-6", catalog); got == nil || got.ID != "claude-opus-5-5" {
		t.Fatalf("template for claude-opus-5-6 = %+v, want claude-opus-5-5", got)
	}
	if got := claudeLiveModelTemplate("claude-mythos-5-9", catalog); got == nil || got.ID != "claude-sonnet-5-5" {
		t.Fatalf("template for a Mythos without a Fable twin = %+v, want the newest claude- model", got)
	}
	if got := claudeLiveModelTemplate("claude-opus-5-20260101", catalog); got == nil || got.ID != "claude-opus-5" {
		t.Fatalf("template for a dated snapshot = %+v, want its undated catalog entry claude-opus-5", got)
	}

	twins := []*ModelInfo{
		{ID: "claude-fable-5-1", Created: 1},
		{ID: "claude-fable-5-2", Created: 2},
		{ID: "claude-haiku-5-5", Created: 9},
	}
	if got := claudeLiveModelTemplate("claude-mythos-5-1-20260901", twins); got == nil || got.ID != "claude-fable-5-1" {
		t.Fatalf("template for a dated Mythos = %+v, want claude-fable-5-1", got)
	}
	if got := claudeLiveModelTemplate("claude-mythos-5-3", twins); got == nil || got.ID != "claude-fable-5-2" {
		t.Fatalf("template for a Mythos without an exact twin = %+v, want the closest Fable", got)
	}
}

func TestClaudeLiveModelsURL(t *testing.T) {
	if got := claudeLiveModelsURL(newClaudeLiveOAuthAuth("a", "")); got != "https://api.anthropic.com/v1/models?limit=1000" {
		t.Fatalf("default URL = %s", got)
	}
	auth := newClaudeLiveOAuthAuth("a", "")
	auth.Attributes["base_url"] = "https://gateway.example/"
	if got := claudeLiveModelsURL(auth); got != "https://gateway.example/v1/models?limit=1000" {
		t.Fatalf("base_url URL = %s", got)
	}
}

func TestClaudeLiveModelsEligible(t *testing.T) {
	apiKey := newClaudeLiveOAuthAuth("key", "")
	apiKey.Attributes = map[string]string{"auth_kind": "apikey", "api_key": "sk"}
	disabled := newClaudeLiveOAuthAuth("disabled", "")
	disabled.Disabled = true
	noToken := newClaudeLiveOAuthAuth("no-token", "")
	delete(noToken.Metadata, "access_token")
	codex := newClaudeLiveOAuthAuth("codex", "")
	codex.Provider = "codex"

	if !claudeLiveModelsEligible(newClaudeLiveOAuthAuth("ok", "")) {
		t.Fatal("Claude OAuth credential with a token should be eligible")
	}
	for _, auth := range []*coreauth.Auth{apiKey, disabled, noToken, codex, nil} {
		if claudeLiveModelsEligible(auth) {
			t.Fatalf("credential %+v should not be eligible", auth)
		}
	}
}

func TestClaudeLiveModelSync_RegistersMythosOnEntitledCredentialOnly(t *testing.T) {
	fake := installClaudeLiveFake(t)
	cvp := newClaudeLiveOAuthAuth("claude-live-cvp", "cvp@example.com")
	plain := newClaudeLiveOAuthAuth("claude-live-plain", "plain@example.com")
	fake.set(cvp.ID, claudeLiveJSON(claudeLiveMythosList))
	fake.set(plain.ID, claudeLiveJSON(claudeLivePlainList))
	service := newClaudeLiveService(t, true, cvp, plain)

	reg := GlobalModelRegistry()
	if reg.ClientSupportsModel(cvp.ID, "claude-mythos-5-1") {
		t.Fatal("Mythos registered before any read")
	}

	runClaudeLiveSync(service, newClaudeLiveModelSyncState(), time.Now())

	if !reg.ClientSupportsModel(cvp.ID, "claude-mythos-5-1") {
		t.Fatal("entitled credential did not get claude-mythos-5-1")
	}
	if reg.ClientSupportsModel(plain.ID, "claude-mythos-5-1") {
		t.Fatal("credential without Mythos access got claude-mythos-5-1")
	}
	if !reg.ClientSupportsModel(plain.ID, "claude-fable-5-1") {
		t.Fatal("catalog model missing from the other credential")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(fake.requests))
	}
	req := fake.requests[0]
	if req.Method != http.MethodGet || req.URL.String() != "https://api.anthropic.com/v1/models?limit=1000" {
		t.Fatalf("request = %s %s", req.Method, req.URL)
	}
	if req.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" || req.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("headers = %v", req.Header)
	}
	if !strings.HasPrefix(req.Header.Get("User-Agent"), "claude-cli/") {
		t.Fatalf("User-Agent = %q, want the Claude Code profile", req.Header.Get("User-Agent"))
	}
}

func TestClaudeLiveModelSync_ScheduleAndFailureBackoff(t *testing.T) {
	fake := installClaudeLiveFake(t)
	cvp := newClaudeLiveOAuthAuth("claude-live-schedule", "cvp@example.com")
	fake.set(cvp.ID, claudeLiveJSON(claudeLiveMythosList))
	service := newClaudeLiveService(t, true, cvp)
	state := newClaudeLiveModelSyncState()
	start := time.Now()
	reg := GlobalModelRegistry()

	runClaudeLiveSync(service, state, start)
	runClaudeLiveSync(service, state, start.Add(claudeLiveModelsTick))
	if got := len(fake.requests); got != 1 {
		t.Fatalf("requests before the interval = %d, want 1", got)
	}

	fake.set(cvp.ID, claudeLiveStatus(http.StatusForbidden))
	runClaudeLiveSync(service, state, start.Add(claudeLiveModelsInterval))
	if got := len(fake.requests); got != 2 {
		t.Fatalf("requests after the interval = %d, want 2", got)
	}
	if !reg.ClientSupportsModel(cvp.ID, "claude-mythos-5-1") {
		t.Fatal("a failed read dropped the models found before")
	}

	fake.set(cvp.ID, claudeLiveJSON(`{"type":"error"}`))
	runClaudeLiveSync(service, state, start.Add(claudeLiveModelsInterval+claudeLiveModelsTick))
	if got := len(fake.requests); got != 2 {
		t.Fatalf("read retried before the backoff: requests = %d", got)
	}
	runClaudeLiveSync(service, state, start.Add(claudeLiveModelsInterval+claudeLiveModelsRetry))
	if got := len(fake.requests); got != 3 {
		t.Fatalf("requests after the backoff = %d, want 3", got)
	}
	if !reg.ClientSupportsModel(cvp.ID, "claude-mythos-5-1") {
		t.Fatal("an answer without a model list dropped the models found before")
	}

	fake.set(cvp.ID, claudeLiveJSON(claudeLivePlainList))
	runClaudeLiveSync(service, state, start.Add(2*claudeLiveModelsInterval+claudeLiveModelsRetry))
	if reg.ClientSupportsModel(cvp.ID, "claude-mythos-5-1") {
		t.Fatal("Mythos kept after Anthropic stopped listing it")
	}
}

func TestClaudeLiveModelSync_StalledReadBlocksOnlyItsCredential(t *testing.T) {
	fake := installClaudeLiveFake(t)
	stalled := newClaudeLiveOAuthAuth("claude-live-stalled", "stalled@example.com")
	healthy := newClaudeLiveOAuthAuth("claude-live-healthy", "healthy@example.com")
	release := make(chan struct{})
	var stalledCalls int
	fake.set(stalled.ID, func() (*http.Response, error) {
		fake.mu.Lock()
		stalledCalls++
		fake.mu.Unlock()
		<-release
		return claudeLiveJSON(claudeLivePlainList)()
	})
	fake.set(healthy.ID, claudeLiveJSON(claudeLiveMythosList))
	service := newClaudeLiveService(t, true, stalled, healthy)
	state := newClaudeLiveModelSyncState()
	start := time.Now()

	service.syncClaudeLiveModels(context.Background(), state, start)
	deadline := time.After(5 * time.Second)
	for !GlobalModelRegistry().ClientSupportsModel(healthy.ID, "claude-mythos-5-1") {
		select {
		case <-deadline:
			close(release)
			t.Fatal("healthy credential waited on the stalled one")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	service.syncClaudeLiveModels(context.Background(), state, start.Add(2*claudeLiveModelsInterval))
	close(release)
	state.reads.Wait()
	if stalledCalls != 1 {
		t.Fatalf("stalled credential read %d times, want 1 while its read is in flight", stalledCalls)
	}
}

func TestClaudeLiveModelSync_DisablingRemovesAdditions(t *testing.T) {
	fake := installClaudeLiveFake(t)
	cvp := newClaudeLiveOAuthAuth("claude-live-toggle", "cvp@example.com")
	fake.set(cvp.ID, claudeLiveJSON(claudeLiveMythosList))
	service := newClaudeLiveService(t, true, cvp)
	state := newClaudeLiveModelSyncState()
	reg := GlobalModelRegistry()

	runClaudeLiveSync(service, state, time.Now())
	if !reg.ClientSupportsModel(cvp.ID, "claude-mythos-5-1") {
		t.Fatal("Mythos not registered while enabled")
	}

	disabled := &config.Config{}
	service.cfgMu.Lock()
	service.cfg = disabled
	service.cfgMu.Unlock()
	runClaudeLiveSync(service, state, time.Now())
	if reg.ClientSupportsModel(cvp.ID, "claude-mythos-5-1") {
		t.Fatal("Mythos kept after live-models was turned off")
	}
}

func TestClaudeLiveModels_ReplacedAccountDoesNotInherit(t *testing.T) {
	fake := installClaudeLiveFake(t)
	cvp := newClaudeLiveOAuthAuth("claude-live-replaced", "cvp@example.com")
	fake.set(cvp.ID, claudeLiveJSON(claudeLiveMythosList))
	service := newClaudeLiveService(t, true, cvp)
	runClaudeLiveSync(service, newClaudeLiveModelSyncState(), time.Now())

	other := newClaudeLiveOAuthAuth(cvp.ID, "someone-else@example.com")
	if _, errUpdate := service.coreManager.Update(context.Background(), other); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}
	service.registerModelsForAuth(context.Background(), other)
	if GlobalModelRegistry().ClientSupportsModel(other.ID, "claude-mythos-5-1") {
		t.Fatal("another account's login inherited Mythos from the file it replaced")
	}
}

func TestClaudeLiveModels_APIKeyCredentialIgnored(t *testing.T) {
	cfg := &config.Config{}
	cfg.Claude.LiveModels = true
	auth := &coreauth.Auth{ID: "claude-live-apikey", Provider: "claude", Attributes: map[string]string{"auth_kind": "apikey", "api_key": "sk"}}
	service := &Service{cfg: cfg}
	service.claudeLiveModels.set(claudeLiveModelKey(auth), []*ModelInfo{{ID: "claude-mythos-5-1"}})

	models := service.withClaudeLiveModels(cfg, auth, []*ModelInfo{{ID: "claude-fable-5-1"}})
	if len(models) != 1 {
		t.Fatalf("API-key credential got live models: %+v", models)
	}
}

func TestClaudeLiveModelSync_ServicesKeepSeparateCaches(t *testing.T) {
	fake := installClaudeLiveFake(t)
	first := newClaudeLiveOAuthAuth("claude-live-first-service", "first@example.com")
	second := newClaudeLiveOAuthAuth("claude-live-second-service", "second@example.com")
	fake.set(first.ID, claudeLiveJSON(claudeLiveMythosList))
	fake.set(second.ID, claudeLiveJSON(claudeLivePlainList))
	firstService := newClaudeLiveService(t, true, first)
	secondService := newClaudeLiveService(t, true, second)

	runClaudeLiveSync(firstService, newClaudeLiveModelSyncState(), time.Now())
	runClaudeLiveSync(secondService, newClaudeLiveModelSyncState(), time.Now())

	if len(firstService.claudeLiveModels.get(claudeLiveModelKey(first))) == 0 {
		t.Fatal("another service's sync pruned this service's live models")
	}
	if len(secondService.claudeLiveModels.get(claudeLiveModelKey(first))) != 0 {
		t.Fatal("live models leaked into another service's cache")
	}
}
