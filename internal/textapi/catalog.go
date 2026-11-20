package textapi

import (
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/pool"
)

// Catalog is ModelCatalogService: the models each active account type (and the anonymous route)
// advertises, cached for 5 minutes or until the account mix changes.
type Catalog struct {
	Deps *Deps
	TTL  time.Duration
	Now  func() time.Time // the clock (tests)

	mu        sync.Mutex
	expires   time.Time
	signature string
	anonymous map[string]map[string]any
	byType    map[string]map[string]map[string]any
}

func modelMap(result map[string]any) map[string]map[string]any {
	models := map[string]map[string]any{}
	data, _ := result["data"].([]map[string]any)
	if data == nil {
		if list, ok := result["data"].([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					data = append(data, m)
				}
			}
		}
	}
	for _, item := range data {
		id := strings.TrimSpace(pyStr(item["id"]))
		if _, seen := models[id]; id != "" && !seen {
			models[id] = item
		}
	}
	return models
}

func (c *Catalog) groups() (map[string][]string, string) {
	groups := map[string][]string{}
	for _, account := range c.Deps.Pool.ListAccounts() {
		if status := account["status"]; status == pool.StatusDisabled || status == pool.StatusAbnormal {
			continue
		}
		token := strings.TrimSpace(pyStr(account["access_token"]))
		kind := pool.NormalizeAccountType(account["type"])
		if token != "" && kind != "" {
			groups[kind] = append(groups[kind], token)
		}
	}
	kinds := make([]string, 0, len(groups))
	for kind := range groups {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	signature := ""
	for _, kind := range kinds {
		signature += kind + "=" + strconv.Itoa(len(groups[kind])) + ";"
	}
	return groups, signature
}

func (c *Catalog) fetch(ctx context.Context, token string) (map[string]map[string]any, error) {
	backend, err := c.Deps.NewBackend(token)
	if err != nil {
		return nil, err
	}
	result, err := backend.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	return modelMap(result), nil
}

func (c *Catalog) fetchType(ctx context.Context, kind string, tokens []string) map[string]map[string]any {
	attempted := map[string]bool{}
	var lastErr error
	for _, token := range tokens {
		resolved := c.Deps.Pool.RefreshAccessToken(ctx, token, false, "list_models")
		if resolved == "" {
			resolved = token
		}
		if attempted[resolved] {
			continue
		}
		attempted[resolved] = true
		models, err := c.fetch(ctx, resolved)
		if err == nil {
			return models
		}
		lastErr = err
	}
	if lastErr != nil {
		log.Printf("model_catalog_account_type_failed account_type=%s error=%v", kind, lastErr)
	}
	return nil
}

func (c *Catalog) ensure(ctx context.Context) {
	groups, signature := c.groups()
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if signature == c.signature && now().Before(c.expires) {
		return
	}
	var wg sync.WaitGroup
	var anonymous map[string]map[string]any
	var anonErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		anonymous, anonErr = c.fetch(ctx, "")
	}()
	results := map[string]map[string]map[string]any{}
	var resultsMu sync.Mutex
	for kind, tokens := range groups {
		wg.Add(1)
		go func(kind string, tokens []string) {
			defer wg.Done()
			models := c.fetchType(ctx, kind, tokens)
			resultsMu.Lock()
			results[kind] = models
			resultsMu.Unlock()
		}(kind, tokens)
	}
	wg.Wait()
	if anonErr != nil {
		log.Printf("model_catalog_anonymous_failed error=%v", anonErr)
		anonymous = c.anonymous
	}
	byType := map[string]map[string]map[string]any{}
	for kind, models := range results {
		if models != nil {
			byType[kind] = models
		} else if old, ok := c.byType[kind]; ok {
			byType[kind] = old
		}
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	c.anonymous, c.byType, c.signature, c.expires = anonymous, byType, signature, now().Add(ttl)
}

// ListModels is list_models plus openai_v1_models' image models.
func (c *Catalog) ListModels(ctx context.Context) map[string]any {
	c.ensure(ctx)
	c.mu.Lock()
	union := map[string]map[string]any{}
	for id, item := range c.anonymous {
		union[id] = item
	}
	kinds := make([]string, 0, len(c.byType))
	for kind := range c.byType {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		for id, item := range c.byType[kind] {
			if _, ok := union[id]; !ok {
				union[id] = item
			}
		}
	}
	c.mu.Unlock()
	ids := make([]string, 0, len(union))
	for id := range union {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data := []any{}
	seen := map[string]bool{}
	for _, id := range ids {
		data = append(data, deepCopy(union[id]))
		seen[id] = true
	}
	// openai_v1_models.list_models: image models the accounts can serve
	accounts := c.Deps.Pool.ListAccounts()
	dynamic := map[string]bool{}
	if len(accounts) > 0 {
		for _, model := range imagegen.WebImageModels() {
			dynamic[model] = true
		}
	}
	codexTypes := map[string]bool{}
	for _, account := range accounts {
		if pool.NormalizeSourceType(account["source_type"]) == "codex" {
			if kind := pool.NormalizeAccountType(account["type"]); kind != "" {
				codexTypes[kind] = true
			}
		}
	}
	if codexTypes["Plus"] || codexTypes["Team"] || codexTypes["Pro"] {
		for _, model := range imagegen.CodexImageModels() {
			dynamic[model] = true
		}
	}
	for _, kind := range []string{"Plus", "Team", "Pro"} {
		if codexTypes[kind] {
			for _, model := range imagegen.CodexImageModels() {
				dynamic[strings.ToLower(kind)+"-"+model] = true
			}
		}
	}
	extra := make([]string, 0, len(dynamic))
	for model := range dynamic {
		extra = append(extra, model)
	}
	sort.Strings(extra)
	for _, model := range extra {
		if !seen[model] {
			data = append(data, map[string]any{"id": model, "object": "model", "created": 0, "owned_by": "chatgpt2api",
				"permission": []any{}, "root": model, "parent": nil})
		}
	}
	return map[string]any{"object": "list", "data": data}
}

// RouteForModel is route_for_model.
func (c *Catalog) RouteForModel(ctx context.Context, model string) *pool.ModelRoute {
	model = strings.TrimSpace(model)
	c.ensure(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	route := &pool.ModelRoute{}
	kinds := make([]string, 0, len(c.byType))
	for kind := range c.byType {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		if _, ok := c.byType[kind][model]; ok {
			route.AccountTypes = append(route.AccountTypes, kind)
		}
	}
	_, route.AllowAnonymous = c.anonymous[model]
	return route
}
