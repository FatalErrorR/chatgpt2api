package sources

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/pool"
)

// CPA is CPAConfig + CPAImportService.
type CPA struct {
	store    *store
	Accounts *pool.Service
	Proxy    func() string // the global proxy (proxy_settings.build_session_kwargs)
	running  sync.WaitGroup
}

func normalizePool(raw map[string]any) map[string]any {
	id := clean(raw["id"])
	if id == "" {
		id = newID()
	}
	return map[string]any{"id": id, "name": clean(raw["name"]), "base_url": clean(raw["base_url"]), "secret_key": clean(raw["secret_key"]),
		"import_job": normalizeJob(raw["import_job"], true)}
}

// NewCPA loads cpa_config.json; unfinished import jobs become failed.
func NewCPA(docs *docstore.Store, accounts *pool.Service, proxy func() string) (*CPA, error) {
	s, err := newStore(docs, cpaDocument, normalizePool)
	if err != nil {
		return nil, err
	}
	return &CPA{store: s, Accounts: accounts, Proxy: proxy}, nil
}

// Wait blocks until imports finish (tests).
func (c *CPA) Wait() { c.running.Wait() }

// Sanitize is sanitize_cpa_pool: everything but the secret key.
func SanitizePool(item map[string]any) map[string]any {
	if item == nil {
		return nil
	}
	out := copyMap(item)
	delete(out, "secret_key")
	return out
}

// Pools is list_pools, sanitized.
func (c *CPA) Pools() []map[string]any {
	out := []map[string]any{}
	for _, item := range c.store.list() {
		out = append(out, SanitizePool(item))
	}
	return out
}

func (c *CPA) Get(id string) map[string]any { return c.store.get(id) }

func (c *CPA) Add(name, baseURL, secretKey string) (map[string]any, error) {
	return c.store.add(map[string]any{"name": name, "base_url": baseURL, "secret_key": secretKey})
}

func (c *CPA) Update(id string, updates map[string]any) (map[string]any, error) {
	return c.store.update(id, updates)
}

func (c *CPA) Delete(id string) (bool, error) { return c.store.remove(id) }

func (c *CPA) client() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if c.Proxy != nil {
		if proxy := strings.TrimSpace(c.Proxy()); proxy != "" {
			if parsed, err := url.Parse(proxy); err == nil {
				transport.Proxy = http.ProxyURL(parsed)
			}
		}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}
}

func managementHeaders(secret string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + secret, "Accept": "application/json"}
}

// Files is list_remote_files.
func (c *CPA) Files(ctx context.Context, item map[string]any) ([]map[string]any, error) {
	base, secret := clean(item["base_url"]), clean(item["secret_key"])
	if base == "" || secret == "" {
		return []map[string]any{}, nil
	}
	result, err := doRequest(ctx, c.client(), http.MethodGet, strings.TrimRight(base, "/")+"/v0/management/auth-files", managementHeaders(secret), nil, nil)
	if err != nil {
		return nil, err
	}
	if !result.ok() {
		return nil, fmt.Errorf("remote list failed: HTTP %d", result.status)
	}
	payload, err := result.json()
	if err != nil {
		return nil, err
	}
	body, _ := payload.(map[string]any)
	files, ok := body["files"].([]any)
	if !ok {
		return nil, errors.New("remote list payload is invalid")
	}
	out := []map[string]any{}
	for _, raw := range files {
		file, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := clean(file["name"])
		if name == "" {
			continue
		}
		email := clean(file["email"])
		if email == "" {
			email = clean(file["account"])
		}
		out = append(out, map[string]any{"name": name, "email": email})
	}
	return out, nil
}

// fetchToken is fetch_remote_access_token.
func (c *CPA) fetchToken(ctx context.Context, item map[string]any, name string) (string, string) {
	base, secret, name := clean(item["base_url"]), clean(item["secret_key"]), strings.TrimSpace(name)
	if base == "" || secret == "" || name == "" {
		return "", "invalid request"
	}
	result, err := doRequest(ctx, c.client(), http.MethodGet, strings.TrimRight(base, "/")+"/v0/management/auth-files/download",
		managementHeaders(secret), url.Values{"name": {name}}, nil)
	if err != nil {
		return "", err.Error()
	}
	if !result.ok() {
		return "", fmt.Sprintf("HTTP %d", result.status)
	}
	payload, err := result.json()
	if err != nil {
		return "", err.Error()
	}
	body, ok := payload.(map[string]any)
	if !ok {
		return "", "invalid payload"
	}
	token := clean(body["access_token"])
	if token == "" {
		return "", "missing access_token"
	}
	return token, ""
}

// Import is start_import: fetch the selected files' tokens (16 at a time) in the background.
func (c *CPA) Import(item map[string]any, names []string) (map[string]any, error) {
	selected := []string{}
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			selected = append(selected, name)
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("selected files is required")
	}
	id := clean(item["id"])
	saved := c.store.setJob(id, newJob(len(selected)))
	if saved == nil {
		return nil, errors.New("pool not found")
	}
	c.running.Add(1)
	go func() {
		defer c.running.Done()
		c.run(id, item, selected)
	}()
	return saved["import_job"].(map[string]any), nil
}

func (c *CPA) run(id string, item map[string]any, names []string) {
	c.store.updateJob(id, map[string]any{"status": "running"})
	ctx := context.Background()
	var mu sync.Mutex
	tokens := []string{}
	slots := make(chan struct{}, min(16, max(1, len(names))))
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		slots <- struct{}{}
		go func(name string) {
			defer wg.Done()
			defer func() { <-slots }()
			token, message := c.fetchToken(ctx, item, name)
			mu.Lock()
			defer mu.Unlock()
			if token != "" {
				tokens = append(tokens, token)
			} else {
				if message == "" {
					message = "unknown error"
				}
				c.store.appendError(id, name, message)
			}
			current := c.store.job(id)
			c.store.updateJob(id, map[string]any{"completed": intOf(current["completed"]) + 1, "failed": errorCount(current)})
		}(name)
	}
	wg.Wait()
	finishImport(c.store, id, c.Accounts, tokens, len(names))
}

// Job is the pool's import job.
func (c *CPA) Job(id string) map[string]any { return c.store.job(id) }
