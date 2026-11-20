// Package proxyops is services/proxy_reassign_service.py: check proxies, move accounts off failing
// proxies and disable them, and reassign accounts. Every action plans first; dry runs return the
// plan, and real runs plan again on the server instead of trusting a plan sent back by the client.
package proxyops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/proxylist"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

const (
	checkWorkers  = 16
	logMovesLimit = 300
	testWorkers   = 8
)

// ErrReassign is ReassignError (shown to the user as a 400).
type ErrReassign string

func (e ErrReassign) Error() string { return string(e) }

// Tester checks one proxy against chatgpt.com (proxy_service.test_proxy).
type Tester func(ctx context.Context, proxyURL string) map[string]any

// Ops ties the proxy list to the account pool.
type Ops struct {
	List     *proxylist.List
	Pool     *pool.Service
	Settings *settings.Store
	Log      pool.Logger
	Test     Tester
}

// Entries is the proxy list with account counts (the GET /api/proxies items).
func (o *Ops) Entries() []map[string]any {
	return o.List.Entries(o.Pool.ProxyAccountCounts(), o.Pool.ProxiesInOrder())
}

// Reserve hands out proxies for new accounts under the per-proxy cap.
func (o *Ops) Reserve(candidates []string, count int) ([]string, error) {
	return o.List.Reserve(candidates, count, o.Pool.ProxyAccountCounts, o.Settings.ProxyMaxAccounts())
}

// Release gives back reserved places.
func (o *Ops) Release(urls []string) { o.List.Release(urls) }

func label(entry map[string]any) string {
	if name := strings.TrimSpace(fmt.Sprint(entry["name"])); entry["name"] != nil && name != "" {
		return name
	}
	masked, _ := entry["masked_url"].(string)
	rest := masked
	if _, after, ok := strings.Cut(masked, "://"); ok {
		rest = after
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if rest = strings.TrimRight(rest, "/"); rest != "" {
		return rest
	}
	return masked
}

func (o *Ops) snapshot() ([]map[string]any, []pool.Account) {
	return o.Entries(), o.Pool.ListAccounts()
}

func keyOf(entry map[string]any) string    { text, _ := entry["key"].(string); return text }
func urlOf(entry map[string]any) string    { text, _ := entry["url"].(string); return text }
func countOf(entry map[string]any) int     { n, _ := entry["account_count"].(int); return n }
func disabledOf(entry map[string]any) bool { b, _ := entry["disabled"].(bool); return b }
func abnormalOf(entry map[string]any) bool {
	health, _ := entry["health"].(map[string]any)
	b, _ := health["abnormal"].(bool)
	return b
}
func strOf(value any) string { text, _ := value.(string); return text }

// Plan is the result of _plan.
type Plan struct {
	Moves       []map[string]any
	Unchanged   int
	Targets     []map[string]any
	MaxPerProxy int
}

func (o *Ops) plan(move []pool.Account, entries []map[string]any, targetKeys map[string]bool, excludeKeys map[string]bool) (*Plan, error) {
	byKey := map[string]map[string]any{}
	for _, entry := range entries {
		byKey[keyOf(entry)] = entry
	}
	candidates := []map[string]any{}
	for _, entry := range entries {
		if disabledOf(entry) || excludeKeys[keyOf(entry)] || (targetKeys != nil && !targetKeys[keyOf(entry)]) {
			continue
		}
		if targetKeys == nil && abnormalOf(entry) {
			continue
		}
		candidates = append(candidates, entry)
	}
	if len(move) > 0 && len(candidates) == 0 {
		return nil, ErrReassign("没有可用的代理接收这些账号（代理都已停用、异常或被排除），先添加或启用代理")
	}
	leaving := map[string]int{}
	for _, account := range move {
		if key := strOf(account["proxy_key"]); key != "" {
			leaving[key]++
		}
	}
	reserved := o.List.ReservedCounts()
	maxPer := o.Settings.ProxyMaxAccounts()
	options := make([]string, len(candidates))
	loads := map[string]int{}
	keyByURL := map[string]string{}
	for i, entry := range candidates {
		options[i] = urlOf(entry)
		loads[urlOf(entry)] = countOf(entry) - leaving[keyOf(entry)] + reserved[keyOf(entry)]
		keyByURL[urlOf(entry)] = keyOf(entry)
	}
	picks, err := proxylist.PickLeastLoaded(options, loads, len(move), maxPer)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Moves: []map[string]any{}, Targets: []map[string]any{}, MaxPerProxy: maxPer}
	received := map[string]int{}
	receivedOrder := []string{}
	for i, account := range move {
		target := keyByURL[picks[i]]
		if strOf(account["proxy_key"]) == target {
			plan.Unchanged++
			continue
		}
		if received[target] == 0 {
			receivedOrder = append(receivedOrder, target)
		}
		received[target]++
		from := "清单外代理"
		if source := byKey[strOf(account["proxy_key"])]; source != nil && strOf(account["proxy_key"]) != "" {
			from = label(source)
		} else if strOf(account["proxy"]) == "" {
			from = "未指定"
		}
		plan.Moves = append(plan.Moves, map[string]any{
			"token": account["access_token"], "email": strOf(account["email"]), "from": from, "to": label(byKey[target]), "to_url": picks[i],
		})
	}
	for _, key := range receivedOrder {
		plan.Targets = append(plan.Targets, map[string]any{
			"key": key, "label": label(byKey[key]), "before": countOf(byKey[key]), "after": countOf(byKey[key]) - leaving[key] + received[key],
		})
	}
	sort.SliceStable(plan.Targets, func(i, j int) bool { return plan.Targets[i]["label"].(string) < plan.Targets[j]["label"].(string) })
	return plan, nil
}

// Public drops tokens and raw proxy addresses (what the frontend sees).
func (p *Plan) Public() map[string]any {
	moves := make([]any, len(p.Moves))
	for i, move := range p.Moves {
		moves[i] = map[string]any{"email": move["email"], "from": move["from"], "to": move["to"]}
	}
	targets := make([]any, len(p.Targets))
	for i, target := range p.Targets {
		targets[i] = target
	}
	return map[string]any{"moves": moves, "unchanged": p.Unchanged, "targets": targets, "max_per_proxy": p.MaxPerProxy}
}

func (o *Ops) apply(plan *Plan, reason string) int {
	byToken := map[string]string{}
	order := []string{}
	for _, move := range plan.Moves {
		token := strOf(move["token"])
		byToken[token] = strOf(move["to_url"])
		order = append(order, token)
	}
	changed := o.Pool.SetAccountProxies(byToken, order)
	if len(changed) > 0 && o.Log != nil {
		moves := []any{}
		for i, move := range plan.Moves {
			if i >= logMovesLimit {
				break
			}
			moves = append(moves, map[string]any{"email": move["email"], "from": move["from"], "to": move["to"]})
		}
		o.Log.Add(logs.TypeAccount, fmt.Sprintf("%s：迁移 %d 个账号的代理", reason, len(changed)),
			map[string]any{"reason": reason, "moved": len(changed), "moves": moves})
	}
	return len(changed)
}

// planOrShortfall returns capacity and "no usable proxy" problems as part of a dry-run plan.
func (o *Ops) planOrShortfall(plan func() (*Plan, error)) map[string]any {
	result, err := plan()
	if err == nil {
		return result.Public()
	}
	out := map[string]any{"moves": []any{}, "unchanged": 0, "targets": []any{}, "max_per_proxy": o.Settings.ProxyMaxAccounts(), "error": err.Error()}
	var capacity *proxylist.CapacityError
	if errors.As(err, &capacity) {
		out["shortfall"] = capacity.Shortfall
	}
	return out
}

func keySet(urls []string) map[string]bool {
	keys := map[string]bool{}
	for _, address := range urls {
		if strings.TrimSpace(address) != "" {
			keys[proxylist.Key(address)] = true
		}
	}
	return keys
}

// Retire is retire_proxies: move the accounts off these proxies, then disable them.
func (o *Ops) Retire(urls []string, dryRun bool) (map[string]any, error) {
	keys := keySet(urls)
	if len(keys) == 0 {
		return nil, ErrReassign("请选择要停用的代理")
	}
	entries, accounts := o.snapshot()
	known := map[string]map[string]any{}
	for _, entry := range entries {
		known[keyOf(entry)] = entry
	}
	missing := 0
	for key := range keys {
		if known[key] == nil {
			missing++
		}
	}
	if missing > 0 {
		return nil, ErrReassign(fmt.Sprintf("有 %d 个代理不在清单里，刷新后再试", missing))
	}
	onRetiring := []pool.Account{}
	for _, account := range accounts {
		if keys[strOf(account["proxy_key"])] {
			onRetiring = append(onRetiring, account)
		}
	}
	plan := func() (*Plan, error) { return o.plan(onRetiring, entries, nil, keys) }
	retiring := []map[string]any{}
	for key := range keys {
		retiring = append(retiring, map[string]any{"key": key, "label": label(known[key]), "account_count": countOf(known[key])})
	}
	sort.SliceStable(retiring, func(i, j int) bool { return retiring[i]["label"].(string) < retiring[j]["label"].(string) })
	retiringOut := make([]any, len(retiring))
	for i, item := range retiring {
		retiringOut[i] = item
	}
	if dryRun {
		out := o.planOrShortfall(plan)
		out["retiring"] = retiringOut
		return out, nil
	}
	result, err := plan()
	if err != nil {
		return nil, err
	}
	moved := o.apply(result, fmt.Sprintf("停用 %d 个代理", len(keys)))
	addresses := []string{}
	labels := []any{}
	for _, item := range retiring {
		addresses = append(addresses, urlOf(known[item["key"].(string)]))
		labels = append(labels, item["label"])
	}
	disabled := o.List.SetDisabled(addresses, true)
	if o.Log != nil {
		o.Log.Add(logs.TypeAccount, fmt.Sprintf("停用 %d 个代理", disabled), map[string]any{"proxies": labels})
	}
	out := result.Public()
	out["retiring"] = retiringOut
	out["moved"] = moved
	out["disabled"] = disabled
	return out, nil
}

// Enable is enable_proxies.
func (o *Ops) Enable(urls []string) int {
	changed := o.List.SetDisabled(urls, false)
	if changed > 0 && o.Log != nil {
		byKey := map[string]map[string]any{}
		for _, entry := range o.Entries() {
			byKey[keyOf(entry)] = entry
		}
		labels := []any{}
		for _, address := range urls {
			if entry := byKey[proxylist.Key(address)]; entry != nil {
				labels = append(labels, label(entry))
			}
		}
		o.Log.Add(logs.TypeAccount, fmt.Sprintf("启用 %d 个代理", changed), map[string]any{"proxies": labels})
	}
	return changed
}

// Reassign is reassign_accounts: spread the chosen accounts over the targets (all usable proxies
// when none are given).
func (o *Ops) Reassign(tokens []string, targets []string, dryRun bool) (map[string]any, error) {
	wanted := map[string]bool{}
	for _, token := range tokens {
		if token != "" {
			wanted[token] = true
		}
	}
	if len(wanted) == 0 {
		return nil, ErrReassign("请选择要重新分配代理的账号")
	}
	entries, accounts := o.snapshot()
	move := []pool.Account{}
	for _, account := range accounts {
		if wanted[strOf(account["access_token"])] {
			move = append(move, account)
		}
	}
	if len(move) == 0 {
		return nil, ErrReassign("选中的账号已不存在，刷新后再试")
	}
	targetKeys := keySet(targets)
	if len(targetKeys) == 0 {
		targetKeys = nil
	}
	plan := func() (*Plan, error) { return o.plan(move, entries, targetKeys, map[string]bool{}) }
	if dryRun {
		return o.planOrShortfall(plan), nil
	}
	result, err := plan()
	if err != nil {
		return nil, err
	}
	out := result.Public()
	out["moved"] = o.apply(result, "重新分配代理")
	return out, nil
}

// IsProviderOutage: most proxies failing (or 3+ all with 402) points at the provider, so there
// is no healthy proxy to move accounts to.
func IsProviderOutage(tested int, failures []map[string]any) bool {
	if len(failures) == 0 || tested < 2 {
		return false
	}
	if len(failures)*2 > tested {
		return true
	}
	if len(failures) < 3 {
		return false
	}
	for _, item := range failures {
		if !strings.Contains(fmt.Sprint(item["error"]), "402") {
			return false
		}
	}
	return true
}

func statusOf(result map[string]any) int {
	switch v := result["status"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

func (o *Ops) testAll(ctx context.Context, entries []map[string]any, workers int) []map[string]any {
	results := make([]map[string]any, len(entries))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < min(workers, max(1, len(entries))); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				results[index] = o.Test(ctx, urlOf(entries[index]))
			}
		}()
	}
	for i := range entries {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func (o *Ops) record(entry map[string]any, result map[string]any) (bool, string) {
	status := statusOf(result)
	if status >= 200 && status < 400 {
		o.List.RecordSuccess(urlOf(entry))
		return true, ""
	}
	cause, _ := result["error"].(string)
	if cause == "" {
		cause = fmt.Sprintf("HTTP %d", status)
	}
	o.List.RecordFailure(urlOf(entry), cause)
	return false, cause
}

// TestProxies is api/proxies.py _test_proxies: test the given proxies (all when empty), 8 at a time.
func (o *Ops) TestProxies(ctx context.Context, urls []string) map[string]any {
	wanted := map[string]bool{}
	for _, address := range urls {
		if address = strings.TrimSpace(address); address != "" {
			wanted[address] = true
		}
	}
	targets := []map[string]any{}
	for _, entry := range o.Entries() {
		if len(wanted) == 0 || wanted[urlOf(entry)] {
			targets = append(targets, entry)
		}
	}
	results := o.testAll(ctx, targets, testWorkers)
	byKey := map[string]any{}
	for i, entry := range targets {
		o.record(entry, results[i])
		byKey[keyOf(entry)] = results[i]
	}
	return map[string]any{"results": byKey, "items": o.Entries()}
}

// Check is check_proxies: one round over every enabled proxy (2xx/3xx passes). Failures come with
// a dry-run retire plan unless it looks like a provider outage.
func (o *Ops) Check(ctx context.Context) map[string]any {
	entries, _ := o.snapshot()
	enabled := []map[string]any{}
	for _, entry := range entries {
		if !disabledOf(entry) {
			enabled = append(enabled, entry)
		}
	}
	results := o.testAll(ctx, enabled, checkWorkers)
	failures := []map[string]any{}
	byKey := map[string]any{}
	for i, entry := range enabled {
		byKey[keyOf(entry)] = results[i]
		if ok, cause := o.record(entry, results[i]); !ok {
			failures = append(failures, map[string]any{
				"key": keyOf(entry), "url": urlOf(entry), "label": label(entry), "status": statusOf(results[i]), "error": cause, "account_count": countOf(entry),
			})
		}
	}
	outage := IsProviderOutage(len(enabled), failures)
	var plan any
	if len(failures) > 0 && !outage {
		addresses := make([]string, len(failures))
		for i, item := range failures {
			addresses[i] = item["url"].(string)
		}
		plan, _ = o.Retire(addresses, true)
	}
	public := make([]any, len(failures))
	for i, item := range failures {
		copied := map[string]any{}
		for key, value := range item {
			if key != "url" {
				copied[key] = value
			}
		}
		public[i] = copied
	}
	return map[string]any{"tested": len(enabled), "passed": len(enabled) - len(failures), "failures": public, "outage": outage, "plan": plan, "results": byKey}
}
