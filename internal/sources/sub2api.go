package sources

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/pool"
)

// Sub2API is Sub2APIConfig + Sub2APIImportService.
type Sub2API struct {
	store    *store
	Accounts *pool.Service
	Client   *http.Client
	Now      func() time.Time

	tokenMu sync.Mutex
	tokens  map[string]cachedToken
	running sync.WaitGroup
}

type cachedToken struct {
	token   string
	expires time.Time
}

func normalizeServer(raw map[string]any) map[string]any {
	id := clean(raw["id"])
	if id == "" {
		id = newID()
	}
	return map[string]any{"id": id, "name": clean(raw["name"]), "base_url": clean(raw["base_url"]), "email": clean(raw["email"]),
		"password": clean(raw["password"]), "api_key": clean(raw["api_key"]), "group_id": clean(raw["group_id"]),
		"import_job": normalizeJob(raw["import_job"], true)}
}

// NewSub2API loads sub2api_config.json.
func NewSub2API(docs *docstore.Store, accounts *pool.Service) (*Sub2API, error) {
	s, err := newStore(docs, sub2apiDocument, normalizeServer)
	if err != nil {
		return nil, err
	}
	return &Sub2API{store: s, Accounts: accounts, Client: &http.Client{Timeout: 30 * time.Second}, Now: time.Now, tokens: map[string]cachedToken{}}, nil
}

func (s *Sub2API) Wait() { s.running.Wait() }

// SanitizeServer is sanitize_sub2api_server: no password or api key, plus has_api_key.
func SanitizeServer(item map[string]any) map[string]any {
	if item == nil {
		return nil
	}
	out := copyMap(item)
	delete(out, "password")
	delete(out, "api_key")
	out["has_api_key"] = clean(item["api_key"]) != ""
	return out
}

func (s *Sub2API) Servers() []map[string]any {
	out := []map[string]any{}
	for _, item := range s.store.list() {
		out = append(out, SanitizeServer(item))
	}
	return out
}

func (s *Sub2API) Get(id string) map[string]any { return s.store.get(id) }

func (s *Sub2API) dropToken(id string) {
	s.tokenMu.Lock()
	delete(s.tokens, id)
	s.tokenMu.Unlock()
}

func (s *Sub2API) Add(fields map[string]any) (map[string]any, error) {
	item, err := s.store.add(fields)
	if item != nil {
		s.dropToken(clean(item["id"]))
	}
	return item, err
}

func (s *Sub2API) Update(id string, updates map[string]any) (map[string]any, error) {
	item, err := s.store.update(id, updates)
	s.dropToken(id)
	return item, err
}

func (s *Sub2API) Delete(id string) (bool, error) {
	removed, err := s.store.remove(id)
	if removed {
		s.dropToken(id)
	}
	return removed, err
}

func (s *Sub2API) Job(id string) map[string]any { return s.store.job(id) }

// unwrap is _unwrap_envelope: {code, message, data} -> data.
func unwrap(payload any) any {
	if m, ok := payload.(map[string]any); ok {
		_, hasData := m["data"]
		_, hasCode := m["code"]
		if hasData && hasCode {
			return m["data"]
		}
	}
	return payload
}

// pagedItems is _extract_paged_items.
func pagedItems(payload any) ([]any, int) {
	switch inner := unwrap(payload).(type) {
	case []any:
		return inner, len(inner)
	case map[string]any:
		for _, key := range []string{"items", "data", "list"} {
			if list, ok := inner[key].([]any); ok {
				total := intOf(inner["total"])
				if total == 0 {
					total = len(list)
				}
				return list, total
			}
		}
	}
	return nil, 0
}

func (s *Sub2API) login(ctx context.Context, base, email, password string) (cachedToken, error) {
	result, err := doRequest(ctx, s.Client, http.MethodPost, strings.TrimRight(base, "/")+"/api/v1/auth/login",
		map[string]string{"Accept": "application/json"}, nil, map[string]any{"email": email, "password": password})
	if err != nil {
		return cachedToken{}, err
	}
	if !result.ok() {
		return cachedToken{}, fmt.Errorf("sub2api login failed: HTTP %d %s", result.status, result.text(200))
	}
	payload, err := result.json()
	if err != nil {
		return cachedToken{}, err
	}
	body, ok := unwrap(payload).(map[string]any)
	if !ok {
		return cachedToken{}, errors.New("sub2api login payload is invalid")
	}
	token := clean(body["access_token"])
	if token == "" {
		return cachedToken{}, errors.New("sub2api login did not return access_token")
	}
	expiresIn := intOf(body["expires_in"])
	if expiresIn == 0 {
		expiresIn = 3600
	}
	return cachedToken{token, s.Now().Add(time.Duration(max(60, expiresIn))*time.Second - tokenSkew)}, nil
}

// authHeaders is _auth_headers: the API key, or a cached admin login token.
func (s *Sub2API) authHeaders(ctx context.Context, item map[string]any) (map[string]string, error) {
	if key := clean(item["api_key"]); key != "" {
		return map[string]string{"x-api-key": key, "Accept": "application/json"}, nil
	}
	email, password := clean(item["email"]), clean(item["password"])
	if email == "" || password == "" {
		return nil, errors.New("sub2api server requires email+password or api_key")
	}
	id := clean(item["id"])
	s.tokenMu.Lock()
	cached, ok := s.tokens[id]
	s.tokenMu.Unlock()
	if ok && cached.expires.After(s.Now()) {
		return map[string]string{"Authorization": "Bearer " + cached.token, "Accept": "application/json"}, nil
	}
	fresh, err := s.login(ctx, clean(item["base_url"]), email, password)
	if err != nil {
		return nil, err
	}
	s.tokenMu.Lock()
	s.tokens[id] = fresh
	s.tokenMu.Unlock()
	return map[string]string{"Authorization": "Bearer " + fresh.token, "Accept": "application/json"}, nil
}

func (s *Sub2API) paged(ctx context.Context, item map[string]any, path string, params url.Values, what string, each func(map[string]any)) error {
	base := clean(item["base_url"])
	if base == "" {
		return nil
	}
	headers, err := s.authHeaders(ctx, item)
	if err != nil {
		return err
	}
	for page := 1; ; page++ {
		query := url.Values{}
		for k, v := range params {
			query[k] = v
		}
		query.Set("page", strconv.Itoa(page))
		query.Set("page_size", "200")
		result, err := doRequest(ctx, s.Client, http.MethodGet, strings.TrimRight(base, "/")+path, headers, query, nil)
		if err != nil {
			return err
		}
		if !result.ok() {
			return fmt.Errorf("sub2api %s failed: HTTP %d %s", what, result.status, result.text(200))
		}
		payload, err := result.json()
		if err != nil {
			return err
		}
		data, total := pagedItems(payload)
		if len(data) == 0 {
			return nil
		}
		for _, raw := range data {
			if m, ok := raw.(map[string]any); ok {
				each(m)
			}
		}
		if page*200 >= total || len(data) < 200 {
			return nil
		}
	}
}

// Accounts is list_remote_accounts: the server's OpenAI OAuth accounts.
func (s *Sub2API) RemoteAccounts(ctx context.Context, item map[string]any) ([]map[string]any, error) {
	params := url.Values{"platform": {"openai"}, "type": {"oauth"}}
	if group := clean(item["group_id"]); group != "" {
		params.Set("group", group)
	}
	out := []map[string]any{}
	err := s.paged(ctx, item, "/api/v1/admin/accounts", params, "list", func(account map[string]any) {
		credentials, _ := account["credentials"].(map[string]any)
		if account["id"] == nil {
			return
		}
		email := clean(credentials["email"])
		if email == "" {
			email = clean(account["name"])
		}
		out = append(out, map[string]any{"id": clean(account["id"]), "name": clean(account["name"]), "email": email,
			"plan_type": clean(credentials["plan_type"]), "status": clean(account["status"]), "expires_at": clean(credentials["expires_at"]),
			"has_refresh_token": clean(credentials["refresh_token"]) != ""})
	})
	return out, err
}

// Groups is list_remote_groups.
func (s *Sub2API) Groups(ctx context.Context, item map[string]any) ([]map[string]any, error) {
	out := []map[string]any{}
	err := s.paged(ctx, item, "/api/v1/admin/groups", url.Values{}, "groups", func(group map[string]any) {
		if group["id"] == nil {
			return
		}
		out = append(out, map[string]any{"id": clean(group["id"]), "name": clean(group["name"]), "description": clean(group["description"]),
			"platform": clean(group["platform"]), "status": clean(group["status"]), "account_count": intOf(group["account_count"]),
			"active_account_count": intOf(group["active_account_count"])})
	})
	return out, err
}

// exportTokens is _fetch_access_tokens_for_accounts.
func (s *Sub2API) exportTokens(ctx context.Context, item map[string]any, ids []string) ([]string, []map[string]any, error) {
	headers, err := s.authHeaders(ctx, item)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	result, err := doRequest(ctx, s.Client, http.MethodGet, strings.TrimRight(clean(item["base_url"]), "/")+"/api/v1/admin/accounts/data", headers,
		url.Values{"ids": {strings.Join(ids, ",")}, "timezone": {"Asia/Shanghai"}}, nil)
	if err != nil {
		return nil, nil, err
	}
	if !result.ok() {
		return nil, nil, fmt.Errorf("HTTP %d", result.status)
	}
	payload, err := result.json()
	if err != nil {
		return nil, nil, err
	}
	data, _ := unwrap(payload).(map[string]any)
	accounts, ok := data["accounts"].([]any)
	if !ok {
		return nil, nil, errors.New("invalid export payload")
	}
	tokens, errs := []string{}, []map[string]any{}
	for _, raw := range accounts {
		account, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		credentials, _ := account["credentials"].(map[string]any)
		token := ""
		for _, key := range []string{"access_token", "accessToken", "token"} {
			if token = clean(credentials[key]); token != "" {
				break
			}
		}
		if token != "" {
			tokens = append(tokens, token)
			continue
		}
		name := clean(account["id"])
		for _, alt := range []string{clean(credentials["chatgpt_account_id"]), clean(account["name"]), "unknown"} {
			if name == "" {
				name = alt
			}
		}
		errs = append(errs, map[string]any{"name": name, "error": "missing access_token"})
	}
	if len(accounts) < len(ids) {
		errs = append(errs, map[string]any{"name": strings.Join(ids, ","), "error": fmt.Sprintf("exported %d/%d accounts", len(accounts), len(ids))})
	}
	return tokens, errs, nil
}

// Import is start_import.
func (s *Sub2API) Import(item map[string]any, accountIDs []string) (map[string]any, error) {
	ids := []string{}
	for _, id := range accountIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("account ids is required")
	}
	id := clean(item["id"])
	saved := s.store.setJob(id, newJob(len(ids)))
	if saved == nil {
		return nil, errors.New("server not found")
	}
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		s.run(id, item, ids)
	}()
	return saved["import_job"].(map[string]any), nil
}

func (s *Sub2API) run(id string, item map[string]any, ids []string) {
	s.store.updateJob(id, map[string]any{"status": "running"})
	tokens, errs, err := s.exportTokens(context.Background(), item, ids)
	if err != nil {
		message := err.Error()
		if message == "" {
			message = "unknown error"
		}
		for _, accountID := range ids {
			s.store.appendError(id, accountID, message)
		}
		tokens = nil
	} else {
		for _, e := range errs {
			message := clean(e["error"])
			if message == "" {
				message = "unknown error"
			}
			s.store.appendError(id, clean(e["name"]), message)
		}
	}
	s.store.updateJob(id, map[string]any{"completed": len(ids), "failed": errorCount(s.store.job(id))})
	finishImport(s.store, id, s.Accounts, tokens, len(ids))
}
