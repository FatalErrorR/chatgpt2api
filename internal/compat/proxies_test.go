package compat

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/proxylist"
)

// Ported from test/test_proxy_reassign.py (and the batch tests of test_proxy_list.py).

const (
	pA = "http://u:p@10.0.0.1:1000"
	pB = "http://u:p@10.0.0.2:1000"
	pC = "http://u:p@10.0.0.3:1000"
	pD = "http://u:p@10.0.0.4:1000"
)

type layoutItem struct {
	url   string
	count int
}

// seed: accounts t<proxy>-<n> on each proxy, then the proxies (and extras) in the list.
func (h *harness) seed(layout []layoutItem, extra []string, disabled []string) {
	items := []pool.Account{}
	for p, item := range layout {
		for a := 0; a < item.count; a++ {
			items = append(items, pool.Account{"access_token": fmt.Sprintf("t%d-%d", p, a), "email": fmt.Sprintf("u%d%d@example.com", p, a), "proxy": item.url})
		}
	}
	if len(items) > 0 {
		h.deps.Pool.AddAccountItems(items)
	}
	all := []string{}
	for _, item := range layout {
		all = append(all, item.url)
	}
	for _, address := range append(all, extra...) {
		known := false
		for _, entry := range h.deps.ProxyOps.Entries() {
			if entry["key"] == proxylist.Key(address) {
				known = true
			}
		}
		if !known {
			h.deps.ProxyOps.List.Add(address, "")
		}
	}
	if len(disabled) > 0 {
		h.deps.ProxyOps.List.SetDisabled(disabled, true)
	}
}

func (h *harness) counts() map[string]int {
	out := map[string]int{}
	for address, n := range h.deps.Pool.ProxyAccountCounts() {
		out[proxylist.Key(address)] += n
	}
	return out
}

func (h *harness) post(path string, body any, status int) map[string]any {
	h.t.Helper()
	code, out := h.do("POST", path, adminKey, body)
	if code != status {
		h.t.Fatalf("%s: status %d, want %d: %v", path, code, status, out)
	}
	return out
}

func entryFor(items any, address string) map[string]any {
	for _, item := range items.([]any) {
		entry := item.(map[string]any)
		if entry["key"] == proxylist.Key(address) {
			return entry
		}
	}
	return nil
}

func keyCounts(pairs ...any) map[string]int {
	out := map[string]int{}
	for i := 0; i < len(pairs); i += 2 {
		out[proxylist.Key(pairs[i].(string))] = pairs[i+1].(int)
	}
	return out
}

func TestRetireDryRunPlansAndChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 3}, {pB, 1}}, []string{pC, pD}, []string{pD})
	before := h.counts()
	plan := h.post("/api/proxies/retire", map[string]any{"urls": []string{pA}, "dry_run": true}, 200)
	to := []string{}
	for _, move := range plan["moves"].([]any) {
		to = append(to, move.(map[string]any)["to"].(string))
	}
	if !reflect.DeepEqual(to, []string{"10.0.0.3:1000", "10.0.0.2:1000", "10.0.0.3:1000"}) {
		t.Fatalf("moves to %v", to)
	}
	targets := map[string][2]float64{}
	for _, target := range plan["targets"].([]any) {
		item := target.(map[string]any)
		targets[item["label"].(string)] = [2]float64{item["before"].(float64), item["after"].(float64)}
	}
	if !reflect.DeepEqual(targets, map[string][2]float64{"10.0.0.2:1000": {1, 2}, "10.0.0.3:1000": {0, 2}}) {
		t.Fatalf("targets %v", targets)
	}
	raw, _ := json.Marshal(plan["moves"])
	if _, hasToken := plan["moves"].([]any)[0].(map[string]any)["token"]; hasToken || strings.Contains(string(raw), ":p@") {
		t.Fatal("plan leaks tokens or proxy passwords")
	}
	if !reflect.DeepEqual(h.counts(), before) || entryFor(h.post("/api/proxies/enable", map[string]any{"urls": []string{}}, 200)["items"], pA)["disabled"] != false {
		t.Fatal("dry run changed something")
	}
}

func TestRetireApplyMovesThenDisablesAndLogs(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 3}, {pB, 1}}, []string{pC}, nil)
	result := h.post("/api/proxies/retire", map[string]any{"urls": []string{pA}, "dry_run": false}, 200)
	if result["moved"] != float64(3) || result["disabled"] != float64(1) {
		t.Fatalf("result %v", result)
	}
	if !reflect.DeepEqual(h.counts(), keyCounts(pB, 2, pC, 2)) {
		t.Fatalf("counts %v", h.counts())
	}
	entry := entryFor(result["items"], pA)
	if entry["disabled"] != true || entry["account_count"] != float64(0) {
		t.Fatalf("entry %v", entry)
	}
	joined := strings.Join(h.log.summaries, "|")
	if !strings.Contains(joined, "停用 1 个代理：迁移 3 个账号的代理") || !strings.Contains(joined, "|停用 1 个代理") {
		t.Fatalf("logs %v", h.log.summaries)
	}
	reloaded, _ := proxylist.New(h.deps.ProxyOps.List.Store())
	if !reloaded.IsDisabled(pA) {
		t.Fatal("disabled flag not stored")
	}
}

func TestRetireCapacityShortfallPreviewsAndBlocksApply(t *testing.T) {
	h := newHarness(t, map[string]any{"proxy_max_accounts": 2})
	h.seed([]layoutItem{{pA, 3}, {pB, 2}}, []string{pC}, nil)
	plan := h.post("/api/proxies/retire", map[string]any{"urls": []string{pA}, "dry_run": true}, 200)
	if plan["shortfall"] != float64(1) || !strings.Contains(plan["error"].(string), "单代理最多账号数") {
		t.Fatalf("plan %v", plan)
	}
	out := h.post("/api/proxies/retire", map[string]any{"urls": []string{pA}, "dry_run": false}, 400)
	if !strings.Contains(detailError(out), "还差 1 个位置") || h.counts()[proxylist.Key(pA)] != 3 || h.deps.ProxyOps.List.IsDisabled(pA) {
		t.Fatalf("apply must change nothing: %v %v", out, h.counts())
	}
}

func TestAbnormalAndDisabledProxiesNeverReceiveAccounts(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 2}}, []string{pB, pC, pD}, []string{pD})
	for i := 0; i < 3; i++ {
		h.deps.ProxyOps.List.RecordFailure(pB, "boom")
	}
	result := h.post("/api/proxies/retire", map[string]any{"urls": []string{pA}, "dry_run": false}, 200)
	if result["moved"] != float64(2) || !reflect.DeepEqual(h.counts(), keyCounts(pC, 2)) {
		t.Fatalf("result %v counts %v", result, h.counts())
	}
}

func TestNoUsableTargetIsAnError(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 1}}, []string{pB}, []string{pB})
	plan := h.post("/api/proxies/retire", map[string]any{"urls": []string{pA}, "dry_run": true}, 200)
	if !strings.Contains(plan["error"].(string), "没有可用的代理") {
		t.Fatalf("plan %v", plan)
	}
	h.post("/api/proxies/retire", map[string]any{"urls": []string{pA}, "dry_run": false}, 400)
}

func TestRetiringAnEmptyProxyJustDisablesIt(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 1}}, []string{pB}, nil)
	result := h.post("/api/proxies/retire", map[string]any{"urls": []string{pB}, "dry_run": false}, 200)
	if result["moved"] != float64(0) || result["disabled"] != float64(1) {
		t.Fatalf("result %v", result)
	}
}

func TestEnableBringsAProxyBackIntoDistribution(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 1}}, []string{pB}, []string{pB})
	out := h.post("/api/accounts", map[string]any{"tokens": []string{"n1"}, "distribute_proxies": []string{pB}}, 400)
	if !strings.Contains(detailError(out), "都已停用") {
		t.Fatalf("out %v", out)
	}
	if h.post("/api/proxies/enable", map[string]any{"urls": []string{pB}}, 200)["enabled"] != float64(1) {
		t.Fatal("enable")
	}
	h.post("/api/accounts", map[string]any{"tokens": []string{"n1"}, "distribute_proxies": []string{pB}}, 200)
	if h.deps.Pool.GetAccount("n1")["proxy"] != pB {
		t.Fatalf("account %v", h.deps.Pool.GetAccount("n1"))
	}
}

func TestImportRespectsThePerProxyCapAndSkipsDisabled(t *testing.T) {
	h := newHarness(t, map[string]any{"proxy_max_accounts": 2})
	h.seed([]layoutItem{{pA, 1}}, []string{pB, pC}, []string{pC})
	h.post("/api/accounts", map[string]any{"tokens": []string{"n1", "n2", "n3"}, "distribute_proxies": []string{pA, pB, pC}}, 200)
	if !reflect.DeepEqual(h.counts(), keyCounts(pA, 2, pB, 2)) {
		t.Fatalf("counts %v", h.counts())
	}
	out := h.post("/api/accounts", map[string]any{"tokens": []string{"n4"}, "distribute_proxies": []string{pA, pB}}, 400)
	if !strings.Contains(detailError(out), "还差 1 个位置") || h.deps.Pool.GetAccount("n4") != nil {
		t.Fatalf("out %v", out)
	}
	if len(h.deps.ProxyOps.List.ReservedCounts()) != 0 {
		t.Fatal("reservations left behind")
	}
}

func TestReassignSelectedAccountsToAllUsableProxies(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 4}}, []string{pB, pC}, nil)
	tokens := []string{"t0-0", "t0-1", "t0-2", "t0-3"}
	plan := h.post("/api/accounts/reassign-proxy", map[string]any{"tokens": tokens, "dry_run": true}, 200)
	if len(plan["moves"].([]any))+int(plan["unchanged"].(float64)) != 4 || !reflect.DeepEqual(h.counts(), keyCounts(pA, 4)) {
		t.Fatalf("plan %v", plan)
	}
	result := h.post("/api/accounts/reassign-proxy", map[string]any{"tokens": tokens, "dry_run": false}, 200)
	values := []int{}
	for _, n := range h.counts() {
		values = append(values, n)
	}
	sort.Ints(values)
	if !reflect.DeepEqual(values, []int{1, 1, 2}) || int(result["moved"].(float64)) != len(result["moves"].([]any)) || len(result["items"].([]any)) != 4 {
		t.Fatalf("counts %v result %v", values, result)
	}
}

func TestReassignToChosenProxiesOnly(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 2}, {pB, 1}}, []string{pC, pD}, nil)
	h.post("/api/accounts/reassign-proxy", map[string]any{"tokens": []string{"t0-0", "t0-1"}, "proxies": []string{pD}, "dry_run": false}, 200)
	if !reflect.DeepEqual(h.counts(), keyCounts(pB, 1, pD, 2)) {
		t.Fatalf("counts %v", h.counts())
	}
}

func TestReassignUnknownAccountsIsAnError(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 1}}, nil, nil)
	h.post("/api/accounts/reassign-proxy", map[string]any{"tokens": []string{"nope"}, "dry_run": true}, 400)
	h.post("/api/accounts/reassign-proxy", map[string]any{"tokens": []string{}, "dry_run": true}, 400)
}

func TestOneFailedRoundIsOfferedForRetirement(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 2}, {pB, 1}, {pC, 1}, {pD, 0}}, nil, nil)
	h.failing[proxylist.Key(pA)] = "curl: (28) Connection timed out"
	result := h.post("/api/proxies/check", map[string]any{}, 200)
	if result["tested"] != float64(4) || result["passed"] != float64(3) || result["outage"] != false {
		t.Fatalf("result %v", result)
	}
	failures := result["failures"].([]any)
	if len(failures) != 1 || failures[0].(map[string]any)["label"] != "10.0.0.1:1000" {
		t.Fatalf("failures %v", failures)
	}
	if _, leaked := failures[0].(map[string]any)["url"]; leaked {
		t.Fatal("failure leaks the proxy address")
	}
	if len(result["plan"].(map[string]any)["moves"].([]any)) != 2 || h.counts()[proxylist.Key(pA)] != 2 {
		t.Fatalf("plan %v", result["plan"])
	}
	if entryFor(result["items"], pA)["health"].(map[string]any)["failures"] != float64(1) {
		t.Fatal("failure not recorded in health")
	}
}

func TestDisabledProxiesAreNotTested(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 1}}, []string{pB}, []string{pB})
	if h.post("/api/proxies/check", map[string]any{}, 200)["tested"] != float64(1) {
		t.Fatal("disabled proxy was tested")
	}
}

func TestWidespreadFailureIsAProviderOutageWithoutAPlan(t *testing.T) {
	h := newHarness(t)
	h.seed([]layoutItem{{pA, 1}, {pB, 1}, {pC, 1}, {pD, 1}}, nil, nil)
	for _, address := range []string{pA, pB, pC} {
		h.failing[proxylist.Key(address)] = "x"
	}
	result := h.post("/api/proxies/check", map[string]any{}, 200)
	if result["outage"] != true || result["plan"] != nil {
		t.Fatalf("result %v", result)
	}
}

func TestMaxAccountsSettingBounds(t *testing.T) {
	for raw, want := range map[any]int{nil: 4, "6": 6, 0: 1, "x": 4} {
		cfg := map[string]any{}
		if raw != nil {
			cfg["proxy_max_accounts"] = raw
		}
		h := newHarness(t, cfg)
		if got := h.deps.Settings.ProxyMaxAccounts(); got != want {
			t.Errorf("%v: got %d want %d", raw, got, want)
		}
	}
}

// --- test_proxy_list.py ProxyBatchAddTests ---

func (h *harness) batch(text, vendor string, status int) map[string]any {
	return h.post("/api/proxies/batch", map[string]any{"text": text, "vendor": vendor}, status)
}

func addedNames(result map[string]any) []string {
	out := []string{}
	for _, item := range result["added"].([]any) {
		out = append(out, item.(map[string]any)["name"].(string))
	}
	return out
}

func TestBatchColonFormatIsStoredAsURLAndPasswordHidden(t *testing.T) {
	h := newHarness(t)
	result := h.batch("194.39.33.194:5903:rgqflcyp:gqnfwz1z3697\n 5.6.7.8:3128 \n\nsocks5://u:p@9.9.9.9:1080", "IPRoyal", 200)
	if !reflect.DeepEqual(addedNames(result), []string{"IPRoyal-1", "IPRoyal-2", "IPRoyal-3"}) {
		t.Fatalf("names %v", addedNames(result))
	}
	_, listed := h.do("GET", "/api/proxies", adminKey, nil)
	names := map[string]any{}
	for _, item := range listed["items"].([]any) {
		entry := item.(map[string]any)
		names[entry["masked_url"].(string)] = entry["name"]
		if strings.Contains(entry["masked_url"].(string), "gqnfwz1z3697") {
			t.Fatal("password shown")
		}
	}
	want := map[string]any{"http://rgqflcyp:***@194.39.33.194:5903": "IPRoyal-1", "http://5.6.7.8:3128": "IPRoyal-2", "socks5://u:***@9.9.9.9:1080": "IPRoyal-3"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v", names)
	}
	if first := listed["items"].([]any)[0].(map[string]any)["url"]; first != "http://rgqflcyp:gqnfwz1z3697@194.39.33.194:5903" {
		t.Fatalf("stored %v", first)
	}
}

func TestBatchNumberingContinuesFromVendorsHighest(t *testing.T) {
	h := newHarness(t)
	h.batch("1.1.1.1:1\n1.1.1.2:1", "IPRoyal", 200)
	h.post("/api/proxies/update", map[string]any{"url": "http://1.1.1.2:1", "name": "iproyal 7"}, 200)
	h.batch("1.1.1.3:1", "Other", 200)
	if got := addedNames(h.batch("1.1.1.4:1\n1.1.1.5:1", "IPRoyal", 200)); !reflect.DeepEqual(got, []string{"IPRoyal-8", "IPRoyal-9"}) {
		t.Fatalf("got %v", got)
	}
	if got := addedNames(h.batch("1.1.1.6:1", "Other", 200)); got[0] != "Other-2" {
		t.Fatalf("got %v", got)
	}
}

func TestBatchDuplicatesAndInvalidLines(t *testing.T) {
	h := newHarness(t)
	h.deps.Pool.AddAccountItems([]pool.Account{{"access_token": "a1", "proxy": "socks5://2.2.2.2:1080"}})
	h.batch("3.3.3.3:80", "V", 200)
	result := h.batch("socks5h://2.2.2.2:1080\n3.3.3.3:80\n4.4.4.4:80:u:p\n4.4.4.4:80:u:p\nnot-a-proxy\n5.5.5.5:99999\nftp://6.6.6.6:21\n7.7.7.7:80", "V", 200)
	added := [][2]string{}
	for _, item := range result["added"].([]any) {
		entry := item.(map[string]any)
		added = append(added, [2]string{entry["name"].(string), entry["masked_url"].(string)})
	}
	if !reflect.DeepEqual(added, [][2]string{{"V-2", "http://u:***@4.4.4.4:80"}, {"V-3", "http://7.7.7.7:80"}}) {
		t.Fatalf("added %v", added)
	}
	skipped := [][2]any{}
	for _, item := range result["skipped"].([]any) {
		entry := item.(map[string]any)
		skipped = append(skipped, [2]any{entry["line"], entry["reason"]})
	}
	if !reflect.DeepEqual(skipped, [][2]any{{float64(1), "已在清单里"}, {float64(2), "已在清单里"}, {float64(4), "本批重复"}}) {
		t.Fatalf("skipped %v", skipped)
	}
	lines := []any{}
	for _, item := range result["invalid"].([]any) {
		lines = append(lines, item.(map[string]any)["line"])
	}
	if !reflect.DeepEqual(lines, []any{float64(5), float64(6), float64(7)}) {
		t.Fatalf("invalid %v", lines)
	}
	// same fields as the Python test: what is shown (masked_url, text); key is the full address by design
	for _, group := range []string{"added", "skipped", "invalid"} {
		for _, item := range result[group].([]any) {
			entry := item.(map[string]any)
			for _, field := range []string{"masked_url", "text"} {
				if text, _ := entry[field].(string); strings.Contains(text, ":p@") {
					t.Fatalf("password shown in %s.%s: %s", group, field, text)
				}
			}
		}
	}
	if entryFor(result["items"], "socks5://2.2.2.2:1080")["account_count"] != float64(1) {
		t.Fatal("the account's proxy entry changed")
	}
}

func TestBatchWithoutVendorKeepsNamesEmpty(t *testing.T) {
	h := newHarness(t)
	if got := addedNames(h.batch("8.8.8.8:80", "", 200)); got[0] != "" {
		t.Fatalf("got %v", got)
	}
}

func TestBatchRejectsEmptyAndOversized(t *testing.T) {
	h := newHarness(t)
	if !strings.Contains(detailError(h.batch(" \n ", "", 400)), "每行一个") {
		t.Fatal("empty")
	}
	lines := []string{}
	for i := 0; i < 501; i++ {
		lines = append(lines, fmt.Sprintf("10.0.%d.%d:80", i/250, i%250))
	}
	if !strings.Contains(detailError(h.batch(strings.Join(lines, "\n"), "", 400)), "500") {
		t.Fatal("oversized")
	}
	if _, listed := h.do("GET", "/api/proxies", adminKey, nil); len(listed["items"].([]any)) != 0 {
		t.Fatal("something was added")
	}
}

func TestColonProxyOnAnAccountIsMaskedInTheList(t *testing.T) {
	h := newHarness(t)
	h.deps.Pool.AddAccountItems([]pool.Account{{"access_token": "c1", "proxy": "194.39.33.194:5903:rgqflcyp:gqnfwz1z3697"}})
	_, listed := h.do("GET", "/api/proxies", adminKey, nil)
	items := listed["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["masked_url"] != "http://rgqflcyp:***@194.39.33.194:5903" || items[0].(map[string]any)["account_count"] != float64(1) {
		t.Fatalf("items %v", items)
	}
	if reason := h.batch("194.39.33.194:5903:rgqflcyp:gqnfwz1z3697", "", 200)["skipped"].([]any)[0].(map[string]any)["reason"]; reason != "已在清单里" {
		t.Fatalf("reason %v", reason)
	}
}
