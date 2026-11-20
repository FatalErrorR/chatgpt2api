package proxylist

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// FailureThreshold consecutive failed requests mark a proxy abnormal; one success clears it.
	FailureThreshold = 3
	maxBatchLines    = 500
	documentName     = "proxies"
)

var proxySchemes = map[string]bool{"http": true, "https": true, "socks": true, "socks4": true, "socks4a": true, "socks5": true, "socks5h": true}

// ErrInput is a user-facing error (HTTP 400), Python's ValueError.
type ErrInput string

func (e ErrInput) Error() string { return string(e) }

// CapacityError means the proxies have too few free places; Shortfall is how many are missing.
type CapacityError struct {
	Shortfall   int
	MaxPerProxy int
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("代理名额不足：还差 %d 个位置。先添加代理，或在设置里调大「单代理最多账号数」（现在是 %d）", e.Shortfall, e.MaxPerProxy)
}

// IsInputError reports whether err should be shown to the user as a 400.
func IsInputError(err error) bool {
	var input ErrInput
	var capacity *CapacityError
	return errors.As(err, &input) || errors.As(err, &capacity)
}

// MaskURL hides the password: scheme://user:pass@host -> scheme://user:***@host.
func MaskURL(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = NormalizeProxyURL(raw)
	}
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	credentials, host := raw[:at], raw[at+1:]
	scheme, userinfo := "", credentials
	if i := strings.LastIndex(credentials, "://"); i >= 0 {
		scheme, userinfo = credentials[:i+3], credentials[i+3:]
	}
	user, _, _ := strings.Cut(userinfo, ":")
	return scheme + user + ":***@" + host
}

// ParseProxyLine turns one line into the address to store (parse_proxy_line).
func ParseProxyLine(line string) (string, error) {
	line = strings.TrimSpace(line)
	address := line
	if !strings.Contains(line, "://") {
		address = NormalizeProxyURL(line)
	}
	if !strings.Contains(address, "://") {
		return "", ErrInput("格式应为 host:port:账号:密码、host:port 或完整代理地址")
	}
	scheme, rest, _ := strings.Cut(address, "://")
	if !proxySchemes[strings.ToLower(scheme)] {
		return "", ErrInput("不支持的代理协议 " + strings.ToLower(scheme))
	}
	host := rest
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	parsed, err := url.Parse("http://" + host)
	if err != nil || parsed.Hostname() == "" {
		return "", ErrInput("缺少主机或端口不正确")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 || port > 65535 {
		return "", ErrInput("缺少主机或端口不正确")
	}
	return address, nil
}

// nextVendorNumber is the vendor's highest number + 1; "厂商-3", "厂商 3" and "厂商3" all count.
func nextVendorNumber(names []string, vendor string) int {
	pattern := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(vendor) + `\s*[-_#]?\s*(\d+)$`)
	highest := 0
	for _, name := range names {
		if match := pattern.FindStringSubmatch(strings.TrimSpace(name)); match != nil {
			if n, err := strconv.Atoi(match[1]); err == nil && n > highest {
				highest = n
			}
		}
	}
	return highest + 1
}

// PickLeastLoaded gives count accounts one by one to the least loaded option (ties in option
// order), at most maxPerProxy each (0 = no limit). All or nothing: a CapacityError assigns none.
func PickLeastLoaded(options []string, loads map[string]int, count int, maxPerProxy int) ([]string, error) {
	current := make(map[string]int, len(options))
	for _, option := range options {
		current[option] = loads[option]
	}
	picks := make([]string, 0, count)
	for i := 0; i < count; i++ {
		best := ""
		for _, option := range options {
			if maxPerProxy > 0 && current[option] >= maxPerProxy {
				continue
			}
			if best == "" || current[option] < current[best] {
				best = option
			}
		}
		if best == "" {
			return nil, &CapacityError{Shortfall: count - len(picks), MaxPerProxy: maxPerProxy}
		}
		current[best]++
		picks = append(picks, best)
	}
	return picks, nil
}

func countsByKey(accountCounts map[string]int) map[string]int {
	counts := map[string]int{}
	for address, count := range accountCounts {
		if address != "" {
			counts[Key(address)] += count
		}
	}
	return counts
}

// Entry is one proxy of the list.
type Entry struct {
	URL      string `json:"url"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled,omitempty"`
}

type health struct {
	successes, failures, consecutive int
	lastError                        string
	lastFailure, lastSuccess         *float64
}

// Persistence stores the list (proxies.json in Python).
type Persistence interface {
	Load(name string, out any) (bool, error)
	Save(name string, value any) error
}

// List is ProxyList.
type List struct {
	mu       sync.Mutex
	store    Persistence
	entries  []*Entry
	health   map[string]*health
	reserved map[string]int
	now      func() time.Time
}

// New loads the list. Duplicate spellings of one proxy keep the first entry.
func New(store Persistence) (*List, error) {
	l := &List{store: store, health: map[string]*health{}, reserved: map[string]int{}, now: time.Now}
	var raw []map[string]any
	if _, err := store.Load(documentName, &raw); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, item := range raw {
		address, _ := item["url"].(string)
		address = strings.TrimSpace(address)
		if address == "" || seen[Key(address)] {
			continue
		}
		seen[Key(address)] = true
		name, _ := item["name"].(string)
		disabled, _ := item["disabled"].(bool)
		l.entries = append(l.entries, &Entry{URL: address, Name: strings.TrimSpace(name), Disabled: disabled})
	}
	return l, nil
}

func (l *List) saveLocked() error {
	out := make([]Entry, len(l.entries))
	for i, entry := range l.entries {
		out[i] = *entry
	}
	return l.store.Save(documentName, out)
}

func (l *List) findLocked(address string) *Entry {
	key := Key(address)
	for _, entry := range l.entries {
		if Key(entry.URL) == key {
			return entry
		}
	}
	return nil
}

func (l *List) healthView(address string) map[string]any {
	h := l.health[Key(address)]
	if h == nil {
		h = &health{}
	}
	var lastFailure, lastSuccess any
	if h.lastFailure != nil {
		lastFailure = *h.lastFailure
	}
	if h.lastSuccess != nil {
		lastSuccess = *h.lastSuccess
	}
	return map[string]any{
		"abnormal": h.consecutive >= FailureThreshold, "successes": h.successes, "failures": h.failures,
		"consecutive_failures": h.consecutive, "last_error": h.lastError, "last_failure_at": lastFailure, "last_success_at": lastSuccess,
	}
}

// Entries is list_entries: the list, with proxies used by accounts but not listed added (unnamed).
func (l *List) Entries(accountCounts map[string]int, accountOrder []string) []map[string]any {
	counts := countsByKey(accountCounts)
	l.mu.Lock()
	defer l.mu.Unlock()
	added := false
	for _, address := range accountOrder {
		if address != "" && l.findLocked(address) == nil {
			l.entries = append(l.entries, &Entry{URL: address})
			added = true
		}
	}
	if added {
		l.saveLocked()
	}
	out := make([]map[string]any, 0, len(l.entries))
	for _, entry := range l.entries {
		out = append(out, map[string]any{
			"url": entry.URL, "name": entry.Name, "disabled": entry.Disabled, "key": Key(entry.URL),
			"masked_url": MaskURL(entry.URL), "account_count": counts[Key(entry.URL)], "health": l.healthView(entry.URL),
		})
	}
	return out
}

// Add appends one proxy.
func (l *List) Add(address, name string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return ErrInput("请填写代理地址")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.findLocked(address) != nil {
		return ErrInput("这个代理已经在清单里")
	}
	l.entries = append(l.entries, &Entry{URL: address, Name: strings.TrimSpace(name)})
	return l.saveLocked()
}

// AddBatch is add_batch: one proxy per line, named "vendor-N" after the vendor's highest number.
func (l *List) AddBatch(text, vendor string) (map[string]any, error) {
	vendor = strings.TrimSpace(vendor)
	type line struct {
		number int
		text   string
	}
	lines := []line{}
	for number, raw := range strings.Split(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n"), "\n") {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			lines = append(lines, line{number + 1, trimmed})
		}
	}
	if len(lines) == 0 {
		return nil, ErrInput("请填写代理，每行一个")
	}
	if len(lines) > maxBatchLines {
		return nil, ErrInput(fmt.Sprintf("一次最多 %d 个代理", maxBatchLines))
	}
	added, skipped, invalid := []*Entry{}, []any{}, []any{}
	l.mu.Lock()
	defer l.mu.Unlock()
	sequence := 0
	if vendor != "" {
		names := make([]string, len(l.entries))
		for i, entry := range l.entries {
			names[i] = entry.Name
		}
		sequence = nextVendorNumber(names, vendor)
	}
	for _, item := range lines {
		address, err := ParseProxyLine(item.text)
		if err != nil {
			invalid = append(invalid, map[string]any{"line": item.number, "text": MaskURL(item.text), "error": err.Error()})
			continue
		}
		if existing := l.findLocked(address); existing != nil {
			reason := "已在清单里"
			for _, entry := range added {
				if entry == existing {
					reason = "本批重复"
				}
			}
			skipped = append(skipped, map[string]any{"line": item.number, "text": MaskURL(item.text), "reason": reason, "name": existing.Name})
			continue
		}
		entry := &Entry{URL: address}
		if vendor != "" {
			entry.Name = fmt.Sprintf("%s-%d", vendor, sequence)
		}
		sequence++
		l.entries = append(l.entries, entry)
		added = append(added, entry)
	}
	if len(added) > 0 {
		if err := l.saveLocked(); err != nil {
			return nil, err
		}
	}
	addedOut := make([]any, len(added))
	for i, entry := range added {
		addedOut[i] = map[string]any{"name": entry.Name, "masked_url": MaskURL(entry.URL), "key": Key(entry.URL)}
	}
	return map[string]any{"added": addedOut, "skipped": skipped, "invalid": invalid}, nil
}

// Rename sets a proxy's name.
func (l *List) Rename(address, name string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.findLocked(strings.TrimSpace(address))
	if entry == nil {
		return ErrInput("代理不存在")
	}
	entry.Name = strings.TrimSpace(name)
	return l.saveLocked()
}

// Delete removes a proxy no account uses.
func (l *List) Delete(address string, accountCounts map[string]int) error {
	if count := countsByKey(accountCounts)[Key(address)]; count > 0 {
		return ErrInput(fmt.Sprintf("还有 %d 个账号在用这个代理，先把它们改到别的代理", count))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.findLocked(strings.TrimSpace(address))
	if entry == nil {
		return ErrInput("代理不存在")
	}
	for i, current := range l.entries {
		if current == entry {
			l.entries = append(l.entries[:i], l.entries[i+1:]...)
			break
		}
	}
	delete(l.health, Key(entry.URL))
	return l.saveLocked()
}

// SetDisabled disables or enables proxies (matched by key) and returns how many changed.
func (l *List) SetDisabled(addresses []string, disabled bool) int {
	keys := map[string]bool{}
	for _, address := range addresses {
		if strings.TrimSpace(address) != "" {
			keys[Key(address)] = true
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := 0
	for _, entry := range l.entries {
		if !keys[Key(entry.URL)] || entry.Disabled == disabled {
			continue
		}
		entry.Disabled = disabled
		changed++
	}
	if changed > 0 {
		l.saveLocked()
	}
	return changed
}

// IsDisabled reports whether the proxy is disabled.
func (l *List) IsDisabled(address string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.findLocked(address)
	return entry != nil && entry.Disabled
}

// ReservedCounts are the places other batches hold, by key.
func (l *List) ReservedCounts() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.reserved))
	for key, value := range l.reserved {
		out[key] = value
	}
	return out
}

func (l *List) stamp() *float64 {
	now := float64(l.now().UnixNano()) / 1e9
	return &now
}

// RecordSuccess clears the failure streak.
func (l *List) RecordSuccess(address string) {
	if address == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.healthFor(address)
	h.successes++
	h.consecutive = 0
	h.lastSuccess = l.stamp()
}

// RecordFailure counts one failed request through the proxy.
func (l *List) RecordFailure(address, cause string) {
	if address == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.healthFor(address)
	h.failures++
	h.consecutive++
	if runes := []rune(cause); len(runes) > 300 {
		cause = string(runes[:300])
	}
	h.lastError = cause
	h.lastFailure = l.stamp()
}

func (l *List) healthFor(address string) *health {
	key := Key(address)
	if l.health[key] == nil {
		l.health[key] = &health{}
	}
	return l.health[key]
}

// IsAbnormal reports a failure streak of FailureThreshold or more.
func (l *List) IsAbnormal(address string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.isAbnormalLocked(address)
}

func (l *List) isAbnormalLocked(address string) bool {
	h := l.health[Key(address)]
	return h != nil && h.consecutive >= FailureThreshold
}

// Reserve is reserve: give count new accounts the least loaded candidates (accounts + other
// batches' reservations + this batch), skipping disabled and, unless all are, abnormal proxies.
// countAccounts is called under the lock. Every result must be passed to Release later.
func (l *List) Reserve(candidates []string, count int, countAccounts func() map[string]int, maxPerProxy int) ([]string, error) {
	seen := map[string]bool{}
	options := []string{}
	for _, address := range candidates {
		address = strings.TrimSpace(address)
		if address != "" && !seen[Key(address)] {
			seen[Key(address)] = true
			options = append(options, address)
		}
	}
	if len(options) == 0 {
		return nil, ErrInput("平均分配至少要选一个代理")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	enabled := []string{}
	for _, address := range options {
		if entry := l.findLocked(address); entry == nil || !entry.Disabled {
			enabled = append(enabled, address)
		}
	}
	if len(enabled) == 0 {
		return nil, ErrInput("所选代理都已停用")
	}
	usable := []string{}
	for _, address := range enabled {
		if !l.isAbnormalLocked(address) {
			usable = append(usable, address)
		}
	}
	if len(usable) == 0 {
		usable = enabled
	}
	counts := countsByKey(countAccounts())
	loads := map[string]int{}
	for _, address := range usable {
		loads[address] = counts[Key(address)] + l.reserved[Key(address)]
	}
	assigned, err := PickLeastLoaded(usable, loads, count, maxPerProxy)
	if err != nil {
		return nil, err
	}
	for _, pick := range assigned {
		l.reserved[Key(pick)]++
	}
	return assigned, nil
}

// Release returns reserved places once the accounts are stored (or failed).
func (l *List) Release(addresses []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, address := range addresses {
		key := Key(address)
		if left := l.reserved[key] - 1; left > 0 {
			l.reserved[key] = left
		} else {
			delete(l.reserved, key)
		}
	}
}

// Store returns the persistence the list was loaded from (tests reload through it).
func (l *List) Store() Persistence { return l.store }
