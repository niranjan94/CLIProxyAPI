package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	claudeLiveModelsInterval   = time.Hour
	claudeLiveModelsRetry      = 10 * time.Minute
	claudeLiveModelsTick       = 5 * time.Minute
	claudeLiveModelsStartDelay = 15 * time.Second
	claudeLiveModelsMaxBody    = 8 << 20
	claudeLiveModelsBaseURL    = "https://api.anthropic.com"
	claudeLiveModelsAPIVersion = "2023-06-01"
	claudeLiveModelsOAuthBeta  = "oauth-2025-04-20"
)

// claudeLiveModelTwins maps a model family Anthropic documents as sharing
// another family's specifications to that family, so a discovered model
// inherits capability metadata from its twin of the same version.
var claudeLiveModelTwins = map[string]string{
	"claude-mythos-": "claude-fable-",
}

// claudeLiveModelsDo sends the model list request; tests replace it.
var claudeLiveModelsDo = func(ctx context.Context, manager *coreauth.Manager, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return manager.HttpRequest(ctx, auth, req)
}

type claudeLiveModel struct {
	id                  string
	displayName         string
	created             int64
	contextLength       int
	maxCompletionTokens int
}

type claudeLiveModelCache struct {
	mu        sync.RWMutex
	additions map[string][]*ModelInfo
}

func (c *claudeLiveModelCache) get(key string) []*ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.additions[key]
}

// set stores the additions for key and reports whether they changed.
func (c *claudeLiveModelCache) set(key string, additions []*ModelInfo) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	previous := c.additions[key]
	changed := (len(previous) > 0 || len(additions) > 0) && !reflect.DeepEqual(previous, additions)
	if len(additions) == 0 {
		delete(c.additions, key)
	} else {
		if c.additions == nil {
			c.additions = make(map[string][]*ModelInfo)
		}
		c.additions[key] = additions
	}
	return changed
}

func (c *claudeLiveModelCache) retain(known map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.additions {
		if _, ok := known[key]; !ok {
			delete(c.additions, key)
		}
	}
}

// claudeLiveModelKey binds discovered models to the credential and the account
// it signs in with, so a credential file replaced by another account's login
// does not inherit the previous account's models.
func claudeLiveModelKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	email := ""
	if auth.Metadata != nil {
		email, _ = auth.Metadata["email"].(string)
	}
	return auth.ID + "\x00" + strings.ToLower(strings.TrimSpace(email))
}

func claudeLiveModelsEnabled(cfg *config.Config) bool {
	return cfg != nil && !cfg.Home.Enabled && cfg.Claude.LiveModels
}

func claudeLiveModelsEligible(auth *coreauth.Auth) bool {
	if auth == nil || auth.Disabled || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || auth.AuthKind() != coreauth.AuthKindOAuth {
		return false
	}
	token, _ := auth.Metadata["access_token"].(string)
	return strings.TrimSpace(token) != ""
}

// withClaudeLiveModels appends the models discovered for the credential that
// the catalog list does not carry. A catalog entry always wins.
func (s *Service) withClaudeLiveModels(cfg *config.Config, auth *coreauth.Auth, models []*ModelInfo) []*ModelInfo {
	if s == nil || !claudeLiveModelsEnabled(cfg) || !claudeLiveModelsEligible(auth) {
		return models
	}
	additions := s.claudeLiveModels.get(claudeLiveModelKey(auth))
	if len(additions) == 0 {
		return models
	}
	known := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			known[model.ID] = struct{}{}
		}
	}
	for _, addition := range additions {
		if _, ok := known[addition.ID]; !ok {
			models = append(models, registry.CloneModelInfo(addition))
		}
	}
	return models
}

type claudeLiveModelSyncState struct {
	mu       sync.Mutex
	nextSync map[string]time.Time
	running  map[string]struct{}
	reads    sync.WaitGroup
}

func newClaudeLiveModelSyncState() *claudeLiveModelSyncState {
	return &claudeLiveModelSyncState{nextSync: make(map[string]time.Time), running: make(map[string]struct{})}
}

// begin reports whether key is due with no read in flight, and marks it running.
func (st *claudeLiveModelSyncState) begin(key string, now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, running := st.running[key]; running {
		return false
	}
	if next, ok := st.nextSync[key]; ok && now.Before(next) {
		return false
	}
	st.running[key] = struct{}{}
	return true
}

func (st *claudeLiveModelSyncState) finish(key string, next time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.running, key)
	st.nextSync[key] = next
}

func (st *claudeLiveModelSyncState) unschedule(key string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.nextSync, key)
}

func (st *claudeLiveModelSyncState) retain(known map[string]struct{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for key := range st.nextSync {
		if _, ok := known[key]; !ok {
			delete(st.nextSync, key)
		}
	}
}

func (s *Service) startClaudeLiveModelSync(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		state := newClaudeLiveModelSyncState()
		// The start delay lets the initial auth scan and token refresh settle.
		timer := time.NewTimer(claudeLiveModelsStartDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			s.syncClaudeLiveModels(ctx, state, time.Now())
			timer.Reset(claudeLiveModelsTick)
		}
	}()
}

// syncClaudeLiveModels starts a read for every due credential. Each read runs
// on its own goroutine without a deadline (upstream calls must not time out), so
// a stalled read holds up only its own credential and ctx ends it on shutdown.
func (s *Service) syncClaudeLiveModels(ctx context.Context, state *claudeLiveModelSyncState, now time.Time) {
	if s == nil || s.coreManager == nil || state == nil {
		return
	}
	enabled := claudeLiveModelsEnabled(s.currentConfig())
	auths := s.coreManager.List()
	known := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if claudeLiveModelsEligible(auth) {
			known[claudeLiveModelKey(auth)] = struct{}{}
		}
	}
	state.retain(known)
	s.claudeLiveModels.retain(known)

	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		if !claudeLiveModelsEligible(auth) {
			continue
		}
		key := claudeLiveModelKey(auth)
		if !enabled {
			if s.claudeLiveModels.set(key, nil) {
				s.refreshModelRegistrationForAuth(auth)
			}
			state.unschedule(key)
			continue
		}
		if !state.begin(key, now) {
			continue
		}
		state.reads.Add(1)
		go func(auth *coreauth.Auth, key string) {
			defer state.reads.Done()
			s.readClaudeLiveModels(ctx, state, auth, key, now)
		}(auth, key)
	}
}

func (s *Service) readClaudeLiveModels(ctx context.Context, state *claudeLiveModelSyncState, auth *coreauth.Auth, key string, now time.Time) {
	discovered, errFetch := s.fetchClaudeLiveModels(ctx, auth)
	if errFetch != nil {
		state.finish(key, now.Add(claudeLiveModelsRetry))
		log.Debugf("claude live models: credential %s: %v", auth.ID, errFetch)
		return
	}
	state.finish(key, now.Add(claudeLiveModelsInterval))

	// The setting may have been turned off, or the credential removed or
	// replaced, while the read was in flight.
	if !claudeLiveModelsEnabled(s.currentConfig()) {
		return
	}
	current, ok := s.coreManager.GetByID(auth.ID)
	if !ok || !claudeLiveModelsEligible(current) || claudeLiveModelKey(current) != key {
		return
	}
	additions := claudeLiveModelAdditionsFor(discovered)
	if !s.claudeLiveModels.set(key, additions) {
		return
	}
	if len(additions) == 0 {
		log.Infof("claude live models: credential %s no longer adds models beyond the catalog", auth.ID)
	} else {
		ids := make([]string, 0, len(additions))
		for _, addition := range additions {
			ids = append(ids, addition.ID)
		}
		log.Infof("claude live models: credential %s adds %d model(s) not in the catalog: %s", auth.ID, len(ids), strings.Join(ids, ", "))
	}
	s.refreshModelRegistrationForAuth(current)
}

func (s *Service) fetchClaudeLiveModels(ctx context.Context, auth *coreauth.Auth) ([]claudeLiveModel, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, claudeLiveModelsURL(auth), nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create request: %w", errRequest)
	}
	helps.ApplyClaudeDefaultDeviceProfileHeaders(req, s.currentConfig())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Version", claudeLiveModelsAPIVersion)
	req.Header.Set("Anthropic-Beta", claudeLiveModelsOAuthBeta)

	resp, errDo := claudeLiveModelsDo(ctx, s.coreManager, auth, req)
	if errDo != nil {
		return nil, fmt.Errorf("request failed: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("claude live models: close response body: %v", errClose)
		}
	}()
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, claudeLiveModelsMaxBody))
	if errRead != nil {
		return nil, fmt.Errorf("read response: %w", errRead)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return parseClaudeLiveModels(body)
}

func claudeLiveModelsURL(auth *coreauth.Auth) string {
	base := claudeLiveModelsBaseURL
	if auth != nil && auth.Attributes != nil {
		if configured := strings.TrimSpace(auth.Attributes["base_url"]); configured != "" {
			base = configured
		}
	}
	return strings.TrimRight(base, "/") + "/v1/models?limit=1000"
}

// parseClaudeLiveModels fails a response without a model list, so a malformed
// answer keeps the previous additions instead of clearing them.
func parseClaudeLiveModels(body []byte) ([]claudeLiveModel, error) {
	data := ojson.GetBytes(body, "data")
	if !data.IsArray() {
		return nil, fmt.Errorf("response has no model list")
	}
	var models []claudeLiveModel
	data.ForEach(func(_, entry ojson.Result) bool {
		id := strings.TrimSpace(entry.Get("id").String())
		if !strings.HasPrefix(strings.ToLower(id), "claude") {
			return true
		}
		model := claudeLiveModel{
			id:                  id,
			displayName:         strings.TrimSpace(entry.Get("display_name").String()),
			contextLength:       int(entry.Get("max_input_tokens").Int()),
			maxCompletionTokens: int(entry.Get("max_tokens").Int()),
		}
		if createdAt := strings.TrimSpace(entry.Get("created_at").String()); createdAt != "" {
			if parsed, errParse := time.Parse(time.RFC3339Nano, createdAt); errParse == nil {
				model.created = parsed.Unix()
			}
		}
		models = append(models, model)
		return true
	})
	return models, nil
}

func claudeLiveModelAdditionsFor(discovered []claudeLiveModel) []*ModelInfo {
	if len(discovered) == 0 {
		return nil
	}
	catalog := registry.GetClaudeModels()
	known := make(map[string]struct{}, len(catalog))
	for _, model := range catalog {
		if model != nil {
			known[model.ID] = struct{}{}
		}
	}
	var additions []*ModelInfo
	for _, model := range discovered {
		if _, ok := known[model.id]; ok {
			continue
		}
		known[model.id] = struct{}{}
		additions = append(additions, claudeLiveModelInfo(model, catalog))
	}
	sort.Slice(additions, func(i, j int) bool { return additions[i].ID < additions[j].ID })
	return additions
}

// claudeLiveModelInfo copies the template's capability metadata, which the
// model list does not report, and overrides what the model list does report.
func claudeLiveModelInfo(model claudeLiveModel, catalog []*ModelInfo) *ModelInfo {
	info := registry.CloneModelInfo(claudeLiveModelTemplate(model.id, catalog))
	if info == nil {
		info = &ModelInfo{Object: "model", OwnedBy: "anthropic", Type: "claude"}
	}
	info.ID = model.id
	info.MetadataModelID = ""
	info.Name = ""
	info.Version = ""
	info.Description = ""
	info.DisplayName = model.displayName
	if info.DisplayName == "" {
		info.DisplayName = model.id
	}
	if model.created > 0 {
		info.Created = model.created
	}
	if model.contextLength > 0 {
		info.ContextLength = model.contextLength
	}
	if model.maxCompletionTokens > 0 {
		info.MaxCompletionTokens = model.maxCompletionTokens
	}
	return info
}

func claudeLiveModelTemplate(id string, catalog []*ModelInfo) *ModelInfo {
	lower := strings.ToLower(id)
	for prefix, twinPrefix := range claudeLiveModelTwins {
		if version, ok := strings.CutPrefix(lower, prefix); ok {
			lower = twinPrefix + version
			break
		}
	}
	for _, name := range []string{lower, trimClaudeSnapshotDate(lower)} {
		for _, candidate := range catalog {
			if candidate != nil && strings.EqualFold(candidate.ID, name) {
				return candidate
			}
		}
	}
	return closestClaudeCatalogModel(trimClaudeSnapshotDate(lower), catalog)
}

// trimClaudeSnapshotDate drops a trailing -YYYYMMDD snapshot date, so a dated
// snapshot matches its undated catalog entry rather than a longer sibling name.
func trimClaudeSnapshotDate(id string) string {
	const dateLen = len("-20060102")
	if len(id) <= dateLen || id[len(id)-dateLen] != '-' {
		return id
	}
	for _, r := range id[len(id)-dateLen+1:] {
		if r < '0' || r > '9' {
			return id
		}
	}
	return id[:len(id)-dateLen]
}

// closestClaudeCatalogModel returns the catalog model sharing the longest name
// prefix with id, preferring the newest among equals.
func closestClaudeCatalogModel(id string, catalog []*ModelInfo) *ModelInfo {
	var closest *ModelInfo
	longest := 0
	for _, candidate := range catalog {
		if candidate == nil {
			continue
		}
		shared := sharedPrefixLength(id, strings.ToLower(candidate.ID))
		if shared > longest || (shared == longest && closest != nil && candidate.Created > closest.Created) {
			closest, longest = candidate, shared
		}
	}
	return closest
}

func sharedPrefixLength(a, b string) int {
	limit := min(len(a), len(b))
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return limit
}
