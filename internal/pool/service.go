package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/proxylist"
	"github.com/basketikun/chatgpt2api/internal/pyval"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

const (
	newAccountInvalidGrace        = 10 * time.Minute
	invalidConfirm                = 30 * time.Second
	accessTokenRefreshSkew        = 24 * time.Hour
	refreshTokenKeepalive         = 3 * 24 * time.Hour
	refreshTokenKeepaliveBackoff  = 6 * time.Hour
	refreshTokenKeepaliveBatch    = 3
	tokenRefreshErrorBackoff      = 5 * time.Minute
	defaultEditUploadCooldown     = 5 * time.Hour
	maxEditUploadCooldown         = 24 * time.Hour
	cumulativeKey                 = "cumulative_total"
	maxImageTokenSelectionAttempt = 20
)

// ErrInvalidToken means the upstream answered 401: the access token no longer works.
var ErrInvalidToken = errors.New("token invalidated")

// Remote talks to ChatGPT on behalf of the pool.
type Remote interface {
	// UserInfo is OpenAIBackendAPI.get_user_info. It returns an error wrapping ErrInvalidToken on 401.
	UserInfo(ctx context.Context, account Account) (Account, error)
	// RefreshAccessToken exchanges a refresh token (auth.openai.com/oauth/token).
	RefreshAccessToken(ctx context.Context, refreshToken string, account Account) (map[string]string, error)
}

// Relogin runs the password login for one account in the background (services/account_service.py
// _password_re_login_thread). progressID is "" outside a batch.
type Relogin func(token, email, password, event, progressID, totpSecret string)

// Logger records account events (log_service.add with LOG_TYPE_ACCOUNT).
type Logger interface {
	Add(kind, summary string, detail map[string]any) error
}

// Service is AccountService.
type Service struct {
	mu   sync.Mutex
	cond *sync.Cond

	order    []string
	items    map[string]Account
	index    int
	inflight map[string]int
	sideline map[string]time.Time
	aliases  map[string]string

	cumulative int
	persisted  map[string]string // last saved JSON per token, to write only changed documents

	refreshMu sync.Mutex

	store    Store
	settings *settings.Store
	log      Logger
	Remote   Remote
	Relogin  Relogin

	progressMu sync.Mutex
	refreshes  map[string]map[string]any
	relogins   map[string]map[string]any

	now func() time.Time
}

// New loads the accounts from the store.
func New(store Store, cfg *settings.Store, log Logger) (*Service, error) {
	s := &Service{
		items: map[string]Account{}, inflight: map[string]int{}, sideline: map[string]time.Time{}, aliases: map[string]string{},
		persisted: map[string]string{}, store: store, settings: cfg, log: log,
		refreshes: map[string]map[string]any{}, relogins: map[string]map[string]any{}, now: time.Now,
	}
	s.cond = sync.NewCond(&s.mu)
	loaded, err := store.Load()
	if err != nil {
		return nil, err
	}
	for _, item := range loaded {
		account := NormalizeAccount(item, "")
		if account == nil {
			continue
		}
		token := account["access_token"].(string)
		if _, seen := s.items[token]; !seen {
			s.order = append(s.order, token)
		}
		s.items[token] = account
		raw, _ := json.Marshal(account)
		s.persisted[token] = string(raw)
	}
	if total, ok := store.LoadInt(cumulativeKey); ok {
		s.cumulative = total
	} else {
		s.cumulative = len(s.items)
	}
	return s, nil
}

func (s *Service) logAdd(summary string, detail map[string]any) {
	if s.log != nil {
		s.log.Add(logs.TypeAccount, summary, detail)
	}
}

// saveLocked writes the documents that changed since the last save (Python rewrote the whole list).
func (s *Service) saveLocked() {
	changed := map[string]Account{}
	present := make(map[string]bool, len(s.order))
	for _, token := range s.order {
		present[token] = true
		raw, _ := json.Marshal(s.items[token])
		if s.persisted[token] != string(raw) {
			changed[token] = s.items[token]
			s.persisted[token] = string(raw)
		}
	}
	var removed []string
	for token := range s.persisted {
		if !present[token] {
			removed = append(removed, token)
			delete(s.persisted, token)
		}
	}
	if len(changed) == 0 && len(removed) == 0 {
		return
	}
	if err := s.store.Save(s.order, changed, removed); err != nil {
		// keep going like Python would after a failed write; the next save retries the same documents
		for token := range changed {
			delete(s.persisted, token)
		}
	}
}

func (s *Service) setLocked(token string, account Account) {
	if _, ok := s.items[token]; !ok {
		s.order = append(s.order, token)
	}
	s.items[token] = account
}

func (s *Service) popLocked(token string) bool {
	if _, ok := s.items[token]; !ok {
		return false
	}
	delete(s.items, token)
	for i, current := range s.order {
		if current == token {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}

func (s *Service) resolveLocked(token string) string {
	token = strings.TrimSpace(token)
	seen := map[string]bool{}
	for token != "" && !seen[token] {
		if _, ok := s.items[token]; ok {
			break
		}
		next, ok := s.aliases[token]
		if !ok {
			break
		}
		seen[token] = true
		token = next
	}
	return token
}

// ResolveAccessToken follows token rotations.
func (s *Service) ResolveAccessToken(token string) string {
	if token == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(token)
}

// ---- reads ----

// GetAccount returns a copy of the account, nil when unknown.
func (s *Service) GetAccount(token string) Account {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyAccount(s.items[s.resolveLocked(token)])
}

// ListTokens returns every access token in order.
func (s *Service) ListTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func (s *Service) viewsLocked() []Account {
	out := make([]Account, 0, len(s.order))
	for _, token := range s.order {
		account := copyAccount(s.items[token])
		account["image_inflight"] = s.inflight[token]
		proxy := str(account["proxy"])
		if proxy != "" {
			account["proxy_key"] = proxylist.Key(proxy)
		} else {
			account["proxy_key"] = ""
		}
		out = append(out, account)
	}
	return out
}

// ListAccounts is list_accounts: copies plus image_inflight and proxy_key.
func (s *Service) ListAccounts() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewsLocked()
}

func (s *Service) tokensWithStatus(status string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, token := range s.order {
		if s.items[token]["status"] == status {
			out = append(out, token)
		}
	}
	return out
}

func (s *Service) ListLimitedTokens() []string { return s.tokensWithStatus(StatusLimited) }
func (s *Service) ListNormalTokens() []string  { return s.tokensWithStatus(StatusNormal) }

// ---- image account selection ----

func (s *Service) readyLocked(filter Filter) []string {
	now := s.now()
	out := []string{}
	for _, token := range s.order {
		account := s.items[token]
		if !isImageAccountAvailable(account) || !filter.matches(account) || filter.Excluded[token] {
			continue
		}
		if until, ok := s.sideline[token]; ok && until.After(now) {
			continue
		}
		out = append(out, token)
	}
	return out
}

func (s *Service) availableLocked(filter Filter) []string {
	limit := max(1, s.settings.ImageAccountConcurrency())
	out := []string{}
	for _, token := range s.readyLocked(filter) {
		if s.inflight[token] < limit {
			out = append(out, token)
		}
	}
	return out
}

func noQuotaMessage(filter Filter) string {
	kind := filter.PlanType
	if kind == "" {
		kind = filter.SourceType
	}
	if kind == "" {
		return "no available image quota"
	}
	return strings.TrimSpace(strings.ReplaceAll("no available "+kind+" image quota", "  ", " "))
}

// ErrNoQuota carries the "no available image quota" messages of the Python version.
type ErrNoQuota string

func (e ErrNoQuota) Error() string { return string(e) }

// acquireNextLocked waits for a free slot on a ready account, round-robin, and takes it.
func (s *Service) acquireNext(ctx context.Context, filter Filter) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if len(s.readyLocked(filter)) == 0 {
			return "", ErrNoQuota(noQuotaMessage(filter))
		}
		if tokens := s.availableLocked(filter); len(tokens) > 0 {
			token := tokens[s.index%len(tokens)]
			s.index++
			s.inflight[token]++
			return token, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		s.waitLocked(time.Second)
	}
}

// waitLocked is Condition.wait(timeout): released by a broadcast or after the timeout.
func (s *Service) waitLocked(timeout time.Duration) {
	timer := time.AfterFunc(timeout, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	s.cond.Wait()
	timer.Stop()
}

// SidelineImageAccount keeps an account out of image selection for a while, in this process only.
func (s *Service) SidelineImageAccount(token string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sideline[s.resolveLocked(token)] = s.now().Add(d)
	s.cond.Broadcast()
}

// ReleaseImageSlot gives back one concurrency slot.
func (s *Service) ReleaseImageSlot(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveLocked(token)
	if s.inflight[token] <= 1 {
		delete(s.inflight, token)
	} else {
		s.inflight[token]--
	}
	s.cond.Broadcast()
}

// ImageInflight is the slots an account holds right now.
func (s *Service) ImageInflight(token string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[s.resolveLocked(token)]
}

func (s *Service) isEditUploadBlocked(account Account) bool {
	until, ok := parseTime(account["edit_upload_blocked_until"])
	return ok && until.After(s.now().UTC())
}

func (s *Service) editUploadBlockedTokens() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, token := range s.order {
		if s.isEditUploadBlocked(s.items[token]) {
			out[token] = true
		}
	}
	return out
}

var editUploadRetry = regexp.MustCompile(`请\s*(\d+)\s*(小时|分钟)\s*内重试`)

// IsEditUploadThrottled is is_edit_upload_throttled.
func IsEditUploadThrottled(err any) bool {
	text := fmt.Sprint(err)
	if err == nil {
		text = ""
	}
	return strings.Contains(text, "文件上传上限") || strings.Contains(strings.ToLower(text), "file upload limit")
}

// ParseEditUploadCooldown is parse_edit_upload_cooldown.
func ParseEditUploadCooldown(err any) time.Duration {
	match := editUploadRetry.FindStringSubmatch(fmt.Sprint(err))
	if match == nil {
		return defaultEditUploadCooldown
	}
	amount, _ := strconv.Atoi(match[1])
	if amount <= 0 {
		return defaultEditUploadCooldown
	}
	d := time.Duration(amount) * time.Minute
	if match[2] == "小时" {
		d = time.Duration(amount) * time.Hour
	}
	return min(d, maxEditUploadCooldown)
}

// MarkEditUploadCooldown blocks an account from image edits until the upload limit resets.
func (s *Service) MarkEditUploadCooldown(token string, cause any) string {
	if token == "" {
		return ""
	}
	until := isoUTC(s.now().Add(ParseEditUploadCooldown(cause)))
	account := s.UpdateAccount(token, Account{"edit_upload_blocked_until": until}, true)
	s.logAdd("图生图上传冷却", map[string]any{"token": AnonymizeToken(token), "until": until})
	if value := str(account["edit_upload_blocked_until"]); value != "" {
		return value
	}
	return until
}

// GetAvailableAccessToken is get_available_access_token: pick a ready account round-robin, check it
// against ChatGPT (FetchRemoteInfo) and keep its slot; up to 20 tries.
func (s *Service) GetAvailableAccessToken(ctx context.Context, filter Filter, forEdits bool) (string, error) {
	attempted := map[string]bool{}
	for token := range filter.Excluded {
		attempted[token] = true
	}
	skipEditCooldown := forEdits && s.settings.ImageEditUploadCooldownEnabled()
	if skipEditCooldown {
		for token := range s.editUploadBlockedTokens() {
			attempted[token] = true
		}
	}
	for attempt := 0; attempt < maxImageTokenSelectionAttempt; attempt++ {
		candidate := filter
		candidate.Excluded = attempted
		token, err := s.acquireNext(ctx, candidate)
		if err != nil {
			return "", err
		}
		attempted[token] = true
		account, err := s.FetchRemoteInfo(ctx, token, "get_available_access_token", true)
		if err != nil {
			s.ReleaseImageSlot(token)
			continue
		}
		if resolved := str(account["access_token"]); resolved != "" && resolved != token {
			attempted[resolved] = true
		}
		if account != nil && isImageAccountAvailable(account) && filter.matches(account) && !(skipEditCooldown && s.isEditUploadBlocked(account)) {
			if resolved := str(account["access_token"]); resolved != "" {
				return resolved, nil
			}
			return token, nil
		}
		s.ReleaseImageSlot(token)
	}
	message := fmt.Sprintf("no available image quota (tried %d tokens)", len(attempted))
	if kind := firstNonEmpty(filter.PlanType, filter.SourceType); kind != "" {
		message = strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf("no available %s image quota (tried %d tokens)", kind, len(attempted)), "  ", " "))
	}
	return "", ErrNoQuota(message)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ModelRoute says which account types may serve a text model (services/model_service.py).
type ModelRoute struct {
	AccountTypes   []string
	AllowAnonymous bool
}

// ErrModelUnavailable is ModelUnavailableError.
type ErrModelUnavailable string

func (e ErrModelUnavailable) Error() string { return string(e) }

// GetTextAccessToken is get_text_access_token. route is nil for "auto" or unknown routing.
func (s *Service) GetTextAccessToken(ctx context.Context, excluded map[string]bool, model string, route *ModelRoute) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "auto"
	}
	s.mu.Lock()
	candidates := []string{}
	for _, token := range s.order {
		account := s.items[token]
		if account["status"] == StatusDisabled || account["status"] == StatusAbnormal || excluded[token] {
			continue
		}
		if route != nil && !containsString(route.AccountTypes, NormalizeAccountType(account["type"])) {
			continue
		}
		candidates = append(candidates, token)
	}
	if len(candidates) == 0 {
		s.mu.Unlock()
		if route == nil || route.AllowAnonymous {
			return "", nil
		}
		return "", ErrModelUnavailable(fmt.Sprintf("model %s is not available to any active account", pyRepr(model)))
	}
	token := candidates[s.index%len(candidates)]
	s.index++
	s.mu.Unlock()
	if refreshed := s.RefreshAccessToken(ctx, token, false, "get_text_access_token"); refreshed != "" {
		return refreshed, nil
	}
	return token, nil
}

// pyRepr is repr() of a str: single quotes unless the text contains one.
func pyRepr(text string) string {
	if strings.Contains(text, "'") && !strings.Contains(text, `"`) {
		return `"` + text + `"`
	}
	return "'" + strings.ReplaceAll(text, "'", `\'`) + "'"
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// MarkTextUsed stamps last_used_at.
func (s *Service) MarkTextUsed(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveLocked(token)
	current := s.items[token]
	if current == nil {
		return
	}
	next := copyAccount(current)
	next["last_used_at"] = nowLocal()
	if account := NormalizeAccount(next, ""); account != nil {
		s.items[token] = account
		s.saveLocked()
	}
}

// ---- proxies ----

// ProxyAccountCounts counts the accounts on each configured proxy.
func (s *Service) ProxyAccountCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for _, token := range s.order {
		if proxy := str(s.items[token]["proxy"]); proxy != "" {
			counts[proxy]++
		}
	}
	return counts
}

// SetAccountProxies changes several proxies with one save and returns the tokens that changed.
func (s *Service) SetAccountProxies(byToken map[string]string, order []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := []string{}
	if order == nil {
		for token := range byToken {
			order = append(order, token)
		}
	}
	for _, original := range order {
		proxy := strings.TrimSpace(byToken[original])
		token := s.resolveLocked(original)
		account := s.items[token]
		if account == nil || str(account["proxy"]) == proxy {
			continue
		}
		if proxy != "" {
			account["proxy"] = proxy
		} else {
			delete(account, "proxy")
		}
		changed = append(changed, token)
	}
	if len(changed) > 0 {
		s.saveLocked()
	}
	return changed
}

// SameProxyTokens lists the accounts on the same configured proxy (same egress IP).
func (s *Service) SameProxyTokens(token string) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	proxy := s.items[s.resolveLocked(token)]["proxy"]
	if !pyval.Truthy(proxy) {
		return out
	}
	for _, other := range s.order {
		if s.items[other]["proxy"] == proxy {
			out[other] = true
		}
	}
	return out
}

// ---- writes ----

// UpdateAccount merges updates into the account; nil when unknown or auto-removed as 限流.
func (s *Service) UpdateAccount(token string, updates Account, quiet bool) Account {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveLocked(token)
	current := s.items[token]
	if current == nil {
		return nil
	}
	account := NormalizeAccount(merge(current, updates, Account{"access_token": token}), "")
	if account == nil {
		return nil
	}
	if account["status"] == StatusLimited && s.settings.AutoRemoveRateLimitedAccounts() {
		s.popLocked(token)
		s.saveLocked()
		s.logAdd("自动移除限流账号", map[string]any{"token": AnonymizeToken(token)})
		return nil
	}
	s.items[token] = account
	s.saveLocked()
	if !quiet {
		s.logAdd("更新账号", map[string]any{"token": AnonymizeToken(token), "status": account["status"]})
	}
	return copyAccount(account)
}

// prepareAccountPayload is _prepare_account_payload (imports of CPA/Codex files).
func prepareAccountPayload(item Account) Account {
	token := strings.TrimSpace(accountToken(item))
	if token == "" {
		return nil
	}
	payload := copyAccount(item)
	delete(payload, "accessToken")
	payload["access_token"] = token
	if strings.ToLower(str(payload["type"])) == "codex" {
		payload["export_type"] = "codex"
		payload["source_type"] = "codex"
		delete(payload, "type")
	}
	if strings.ToLower(str(payload["export_type"])) == "codex" {
		payload["source_type"] = "codex"
	}
	if pyval.Truthy(payload["plan_type"]) && !pyval.Truthy(payload["type"]) {
		payload["type"] = str(payload["plan_type"])
	}
	return payload
}

// AddResult is what the add methods return.
type AddResult struct {
	Added   int
	Skipped int
	Items   []Account
}

// AddAccountItems imports full account objects.
func (s *Service) AddAccountItems(items []Account) AddResult {
	payloads := []Account{}
	for _, item := range items {
		if payload := prepareAccountPayload(item); payload != nil {
			payloads = append(payloads, payload)
		}
	}
	return s.addPayloads(payloads)
}

// AddAccounts imports bare access tokens.
func (s *Service) AddAccounts(tokens []string, sourceType string) AddResult {
	seen := map[string]bool{}
	payloads := []Account{}
	for _, token := range tokens {
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		payloads = append(payloads, Account{"access_token": token, "source_type": normalizeSourceType(sourceType)})
	}
	if len(payloads) == 0 {
		return AddResult{Items: s.ListAccounts()}
	}
	return s.addPayloads(payloads)
}

func (s *Service) addPayloads(payloads []Account) AddResult {
	order := []string{}
	deduped := map[string]Account{}
	for _, payload := range payloads {
		token := strings.TrimSpace(accountToken(payload))
		if token == "" {
			continue
		}
		if _, ok := deduped[token]; !ok {
			order = append(order, token)
		}
		deduped[token] = merge(deduped[token], payload, Account{"access_token": token})
	}
	if len(order) == 0 {
		return AddResult{Items: s.ListAccounts()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	added, skipped := 0, 0
	for _, token := range order {
		payload := deduped[token]
		current := s.items[token]
		if current == nil {
			added++
			s.cumulative++
			s.store.SaveInt(cumulativeKey, s.cumulative)
			current = Account{"created_at": nowUTC()}
		} else {
			skipped++
		}
		incoming := copyAccount(payload)
		if !pyval.Truthy(incoming["created_at"]) {
			delete(incoming, "created_at")
		}
		accountType := "free"
		if pyval.Truthy(incoming["type"]) {
			accountType = fmt.Sprint(incoming["type"])
		} else if pyval.Truthy(current["type"]) {
			accountType = fmt.Sprint(current["type"])
		}
		if account := NormalizeAccount(merge(current, incoming, Account{"access_token": token, "type": accountType}), ""); account != nil {
			s.setLocked(token, account)
		}
	}
	s.saveLocked()
	items := s.viewsLocked()
	s.logAdd(fmt.Sprintf("新增 %d 个账号，跳过 %d 个", added, skipped), map[string]any{"added": added, "skipped": skipped})
	return AddResult{Added: added, Skipped: skipped, Items: items}
}

// DeleteAccounts removes accounts and returns how many existed.
func (s *Service) DeleteAccounts(tokens []string) (int, []Account) {
	targets := map[string]bool{}
	for _, token := range tokens {
		if token != "" {
			targets[token] = true
		}
	}
	if len(targets) == 0 {
		return 0, s.ListAccounts()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved := map[string]bool{}
	for token := range targets {
		if token = s.resolveLocked(token); token != "" {
			resolved[token] = true
		}
	}
	removed := 0
	for token := range resolved {
		if s.popLocked(token) {
			removed++
		}
		delete(s.inflight, token)
	}
	for old, next := range s.aliases {
		if resolved[old] || resolved[next] {
			delete(s.aliases, old)
		}
	}
	if removed > 0 {
		if len(s.order) > 0 {
			s.index %= len(s.order)
		} else {
			s.index = 0
		}
		s.saveLocked()
		s.logAdd(fmt.Sprintf("删除 %d 个账号", removed), map[string]any{"removed": removed})
	}
	return removed, s.viewsLocked()
}

// MarkImageResult releases the slot and records one image result.
func (s *Service) MarkImageResult(token string, success bool) Account {
	if token == "" {
		return nil
	}
	s.ReleaseImageSlot(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveLocked(token)
	current := s.items[token]
	if current == nil {
		return nil
	}
	next := copyAccount(current)
	next["last_used_at"] = nowLocal()
	if success {
		next["success"] = intOf(next["success"]) + 1
		quota := max(0, intOf(next["quota"])-1)
		next["quota"] = quota
		if quota == 0 {
			next["status"] = StatusLimited
			next["restore_at"] = orNil(next["restore_at"])
		} else if next["status"] == StatusLimited {
			next["status"] = StatusNormal
		}
	} else {
		next["fail"] = intOf(next["fail"]) + 1
	}
	account := NormalizeAccount(next, "")
	if account == nil {
		return nil
	}
	if account["status"] == StatusLimited && s.settings.AutoRemoveRateLimitedAccounts() {
		s.popLocked(token)
		s.saveLocked()
		s.logAdd("自动移除限流账号", map[string]any{"token": AnonymizeToken(token)})
		return nil
	}
	s.items[token] = account
	s.saveLocked()
	return copyAccount(account)
}

// RemoveInvalidToken marks the account 异常 (or deletes it when auto removal is on).
func (s *Service) RemoveInvalidToken(token, event string, quiet bool, cause string) bool {
	updates := Account{"status": StatusAbnormal, "quota": 0}
	if text := truncate(strings.Join(strings.Fields(cause), " "), 400); text != "" {
		updates["last_refresh_error"] = text
		updates["last_refresh_error_at"] = isoUTC(s.now())
	}
	if !s.settings.AutoRemoveInvalidAccounts() {
		s.UpdateAccount(token, updates, quiet)
		return false
	}
	removed, _ := s.DeleteAccounts([]string{token})
	if removed > 0 {
		s.logAdd("自动移除异常账号", map[string]any{"source": event, "token": AnonymizeToken(token)})
	} else if token != "" {
		s.UpdateAccount(token, updates, quiet)
	}
	return removed > 0
}

func truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) > n {
		return string(runes[:n])
	}
	return text
}

func (s *Service) recordRefreshSuccess(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveLocked(token)
	current := s.items[token]
	if current == nil {
		return
	}
	next := copyAccount(current)
	next["invalid_count"] = 0
	next["last_invalid_at"] = nil
	next["last_refresh_error"] = nil
	next["last_refresh_error_at"] = nil
	if account := NormalizeAccount(next, ""); account != nil {
		s.items[token] = account
	}
}

func (s *Service) shouldDeferInvalid(account Account, now time.Time) bool {
	if account == nil {
		return false
	}
	if created, ok := parseTime(account["created_at"]); ok && now.Sub(created) < newAccountInvalidGrace {
		return true
	}
	if intOf(account["invalid_count"]) <= 1 {
		return true
	}
	if last, ok := parseTime(account["last_invalid_at"]); ok && now.Sub(last) < invalidConfirm {
		return true
	}
	return false
}

// recordInvalidSeen returns true when the account should now be marked invalid.
func (s *Service) recordInvalidSeen(token, event, cause string, deferRemoval bool) bool {
	now := s.now().UTC()
	s.mu.Lock()
	token = s.resolveLocked(token)
	current := s.items[token]
	if current == nil {
		s.mu.Unlock()
		return true
	}
	shouldDefer := deferRemoval && s.shouldDeferInvalid(current, now)
	next := copyAccount(current)
	next["invalid_count"] = intOf(next["invalid_count"]) + 1
	next["last_invalid_at"] = isoUTC(now)
	if cause == "" {
		next["last_refresh_error"] = "invalid access token"
	} else {
		next["last_refresh_error"] = cause
	}
	next["last_refresh_error_at"] = isoUTC(now)
	if account := NormalizeAccount(next, ""); account != nil {
		s.items[token] = account
		s.saveLocked()
	}
	s.mu.Unlock()
	if shouldDefer {
		s.logAdd("暂缓标记异常账号", map[string]any{"source": event, "token": AnonymizeToken(token), "error": cause})
		return false
	}
	return true
}

// FetchRemoteInfo is fetch_remote_info: refresh the token if needed, read the account from
// ChatGPT, and handle 401 (one forced refresh, then count it as invalid).
func (s *Service) FetchRemoteInfo(ctx context.Context, token, event string, deferRemoval bool) (Account, error) {
	if token == "" {
		return nil, errors.New("access_token is required")
	}
	if s.Remote == nil {
		return nil, errors.New("no upstream client configured")
	}
	active := s.RefreshAccessToken(ctx, token, false, event+":preflight")
	if active == "" {
		active = token
	}
	result, err := s.Remote.UserInfo(ctx, s.withToken(active))
	if err != nil {
		if !errors.Is(err, ErrInvalidToken) {
			return nil, err
		}
		refreshed := s.RefreshAccessToken(ctx, active, true, event+":invalid_access_token")
		if refreshed != "" && refreshed != active {
			result, err = s.Remote.UserInfo(ctx, s.withToken(refreshed))
			if err != nil {
				if errors.Is(err, ErrInvalidToken) && s.recordInvalidSeen(refreshed, event, err.Error(), deferRemoval) {
					s.RemoveInvalidToken(refreshed, event, false, "")
				}
				return nil, err
			}
			active = refreshed
		} else {
			if s.recordInvalidSeen(active, event, err.Error(), deferRemoval) {
				s.RemoveInvalidToken(active, event, false, "")
			}
			return nil, err
		}
	}
	s.recordRefreshSuccess(active)
	return s.UpdateAccount(active, result, false), nil
}

// withToken gives the remote client the account (for its proxy) under the token to use.
func (s *Service) withToken(token string) Account {
	account := s.GetAccount(token)
	if account == nil {
		account = Account{}
	}
	account["access_token"] = token
	return account
}

// ---- access token refresh ----

func tokenNeedsRefresh(token string, force bool, now time.Time) bool {
	if force {
		return true
	}
	exp := jwtClaimInt(token, "exp")
	if exp <= 0 {
		return false
	}
	return time.Unix(exp, 0).Sub(now) <= accessTokenRefreshSkew
}

func (s *Service) recentTokenRefreshError(account Account, window time.Duration) bool {
	last, ok := parseTime(account["last_token_refresh_error_at"])
	return ok && s.now().UTC().Sub(last) < window
}

func (s *Service) recordTokenRefreshError(token, event, cause string) {
	s.mu.Lock()
	resolved := s.resolveLocked(token)
	if current := s.items[resolved]; current != nil {
		next := copyAccount(current)
		if cause == "" {
			cause = "refresh token failed"
		}
		next["last_token_refresh_error"] = cause
		next["last_token_refresh_error_at"] = isoUTC(s.now())
		if account := NormalizeAccount(next, ""); account != nil {
			s.items[resolved] = account
			s.saveLocked()
		}
	}
	s.mu.Unlock()
	s.logAdd("refresh_token 刷新 access_token 失败", map[string]any{"source": event, "token": AnonymizeToken(token), "error": cause})
}

func (s *Service) applyRefreshedTokens(oldToken string, data map[string]string, event string) string {
	s.mu.Lock()
	old := s.resolveLocked(oldToken)
	current := s.items[old]
	if current == nil {
		s.mu.Unlock()
		return old
	}
	newToken := strings.TrimSpace(data["access_token"])
	if newToken == "" {
		newToken = old
	}
	next := copyAccount(current)
	next["access_token"] = newToken
	if value := strings.TrimSpace(data["refresh_token"]); value != "" {
		next["refresh_token"] = value
	}
	if value := strings.TrimSpace(data["id_token"]); value != "" {
		next["id_token"] = value
	}
	next["last_token_refresh_at"] = isoUTC(s.now())
	for _, key := range []string{"last_token_refresh_error", "last_token_refresh_error_at", "last_invalid_at", "last_refresh_error", "last_refresh_error_at"} {
		next[key] = nil
	}
	next["invalid_count"] = 0
	account := NormalizeAccount(next, "")
	if account == nil {
		s.mu.Unlock()
		return old
	}
	rotated := newToken != old
	if rotated {
		// like the Python dict: pop the old token, the new one goes to the end of the order
		s.popLocked(old)
		s.aliases[old] = newToken
		if held := s.inflight[old]; held > 0 {
			delete(s.inflight, old)
			s.inflight[newToken] += held
		}
	}
	s.setLocked(newToken, account)
	s.saveLocked()
	s.cond.Broadcast()
	s.mu.Unlock()
	s.logAdd("refresh_token 已刷新 access_token", map[string]any{"source": event, "token": AnonymizeToken(newToken), "rotated": rotated})
	return newToken
}

// RefreshAccessToken is refresh_access_token: renew with the refresh token when the access token
// expires within 24 hours (or when forced). Returns the token to use.
func (s *Service) RefreshAccessToken(ctx context.Context, token string, force bool, event string) string {
	if token == "" {
		return ""
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	resolved := s.resolveLocked(token)
	account := copyAccount(s.items[resolved])
	s.mu.Unlock()
	if account == nil {
		return token
	}
	active := firstNonEmpty(str(account["access_token"]), resolved, token)
	if !tokenNeedsRefresh(active, force, s.now()) {
		return active
	}
	refreshToken := str(account["refresh_token"])
	if refreshToken == "" || s.Remote == nil {
		return active
	}
	if !force && s.recentTokenRefreshError(account, tokenRefreshErrorBackoff) {
		return active
	}
	data, err := s.Remote.RefreshAccessToken(ctx, refreshToken, account)
	if err != nil {
		cause := err.Error()
		s.recordTokenRefreshError(active, event, cause)
		if strings.Contains(strings.ToLower(cause), "app_session_terminated") && s.Relogin != nil {
			email, password := str(account["email"]), str(account["password"])
			if email != "" && password != "" {
				go s.Relogin(active, email, password, event, "", str(account["totp_secret"]))
			}
		}
		return active
	}
	return s.applyRefreshedTokens(active, data, event)
}

// ListExpiringAccessTokens lists accounts with a refresh token whose access token expires soon.
func (s *Service) ListExpiringAccessTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{}
	for _, token := range s.order {
		account := s.items[token]
		if str(account["refresh_token"]) != "" && tokenNeedsRefresh(token, false, s.now()) {
			out = append(out, token)
		}
	}
	return out
}

func (s *Service) keepaliveDueAt(account Account, now time.Time) (time.Time, bool) {
	if str(account["refresh_token"]) == "" || account["status"] == StatusDisabled {
		return time.Time{}, false
	}
	if last, ok := parseTime(account["last_token_refresh_error_at"]); ok && now.Sub(last) < refreshTokenKeepaliveBackoff {
		return time.Time{}, false
	}
	anchor, ok := parseTime(account["last_token_refresh_at"])
	if !ok {
		if iat := jwtClaimInt(str(account["access_token"]), "iat"); iat > 0 {
			anchor, ok = time.Unix(iat, 0).UTC(), true
		}
	}
	if !ok {
		anchor, ok = parseTime(account["created_at"])
	}
	if !ok {
		return now, true
	}
	due := anchor.Add(refreshTokenKeepalive)
	return due, !due.After(now)
}

// ListRefreshTokenKeepaliveTokens returns up to 3 accounts whose refresh token should be used.
func (s *Service) ListRefreshTokenKeepaliveTokens() []string {
	now := s.now().UTC()
	type due struct {
		at    time.Time
		token string
	}
	s.mu.Lock()
	items := []due{}
	for _, token := range s.order {
		if at, ok := s.keepaliveDueAt(s.items[token], now); ok && token != "" {
			items = append(items, due{at, token})
		}
	}
	s.mu.Unlock()
	for i := 1; i < len(items); i++ { // stable insertion sort, like Python's sorted
		for j := i; j > 0 && items[j].at.Before(items[j-1].at); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	out := []string{}
	for i := 0; i < len(items) && i < refreshTokenKeepaliveBatch; i++ {
		out = append(out, items[i].token)
	}
	return out
}

// ---- stats ----

// Stats is get_stats.
func (s *Service) Stats() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	quota, success, fail := 0, 0, 0
	byType := map[string]any{}
	for _, token := range s.order {
		account := s.items[token]
		status, _ := account["status"].(string)
		counts[status]++
		if status == StatusNormal {
			quota += max(0, intOf(account["quota"]))
		}
		success += intOf(account["success"])
		fail += intOf(account["fail"])
		typeName := "unknown"
		if value, ok := account["type"]; ok {
			typeName = fmt.Sprint(value)
		}
		current, _ := byType[typeName].(int)
		byType[typeName] = current + 1
	}
	return map[string]any{
		"total": len(s.order), "cumulative_total": s.cumulative, "active": counts[StatusNormal], "limited": counts[StatusLimited],
		"abnormal": counts[StatusAbnormal], "disabled": counts[StatusDisabled], "total_quota": quota,
		"total_success": success, "total_fail": fail, "by_type": byType,
	}
}

// Health is account_health.
func (s *Service) Health() map[string]any {
	stats := s.Stats()
	healthy := stats["active"].(int) > 0
	out := map[string]any{"healthy": healthy, "status": "degraded"}
	if healthy {
		out["status"] = "ok"
	}
	for key, value := range stats {
		out[key] = value
	}
	return out
}
