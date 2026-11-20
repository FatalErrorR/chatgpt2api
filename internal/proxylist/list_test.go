package proxylist

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	A = "http://user:secret@1.1.1.1:8000"
	B = "http://2.2.2.2:8000"
	C = "socks5://3.3.3.3:1080"
)

type memoryDocs struct{ docs map[string][]byte }

func (m *memoryDocs) Load(name string, out any) (bool, error) {
	raw, ok := m.docs[name]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, out)
}

func (m *memoryDocs) Save(name string, value any) error {
	raw, err := json.Marshal(value)
	m.docs[name] = raw
	return err
}

func newList(t *testing.T) (*List, *memoryDocs) {
	t.Helper()
	docs := &memoryDocs{docs: map[string][]byte{}}
	list, err := New(docs)
	if err != nil {
		t.Fatal(err)
	}
	return list, docs
}

func (l *List) failTimes(address string, times int) {
	for i := 0; i < times; i++ {
		l.RecordFailure(address, "boom")
	}
}

func counts(pairs ...any) (map[string]int, []string) {
	out := map[string]int{}
	order := []string{}
	for i := 0; i < len(pairs); i += 2 {
		out[pairs[i].(string)] = pairs[i+1].(int)
		order = append(order, pairs[i].(string))
	}
	return out, order
}

func none() map[string]int { return map[string]int{} }

func mustReserve(t *testing.T, l *List, candidates []string, n int, accounts func() map[string]int) []string {
	t.Helper()
	got, err := l.Reserve(candidates, n, accounts, 0)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMaskHidesOnlyThePassword(t *testing.T) {
	for in, want := range map[string]string{A: "http://user:***@1.1.1.1:8000", "user:pw@host:1": "user:***@host:1", B: B,
		"194.39.33.194:5903:rgqflcyp:gqnfwz1z3697": "http://rgqflcyp:***@194.39.33.194:5903"} {
		if got := MaskURL(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestListSeedsAccountProxiesAndKeepsNames(t *testing.T) {
	l, docs := newList(t)
	l.Add(B, "美西")
	c, order := counts(A, 2, B, 1)
	items := l.Entries(c, order)
	got := [][3]any{}
	for _, item := range items {
		got = append(got, [3]any{item["url"], item["name"], item["account_count"]})
	}
	if !reflect.DeepEqual(got, [][3]any{{B, "美西", 1}, {A, "", 2}}) || items[1]["masked_url"] != "http://user:***@1.1.1.1:8000" {
		t.Fatalf("items %v", got)
	}
	reloaded, _ := New(docs)
	again := reloaded.Entries(none(), nil)
	if len(again) != 2 || again[0]["name"] != "美西" || again[1]["url"] != A {
		t.Fatalf("reloaded %v", again)
	}
}

func TestRenameAddAndDeleteRules(t *testing.T) {
	l, _ := newList(t)
	if err := l.Add(A, ""); err != nil {
		t.Fatal(err)
	}
	if err := l.Add(A, ""); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := l.Add("  ", ""); err == nil {
		t.Fatal("blank accepted")
	}
	l.Rename(A, " 主力 ")
	if l.Entries(none(), nil)[0]["name"] != "主力" {
		t.Fatal("rename")
	}
	if err := l.Delete(A, map[string]int{A: 2}); err == nil || !strings.Contains(err.Error(), "2 个账号") {
		t.Fatalf("delete in use: %v", err)
	}
	if err := l.Delete(A, map[string]int{B: 1}); err != nil {
		t.Fatal(err)
	}
	if len(l.Entries(none(), nil)) != 0 {
		t.Fatal("not deleted")
	}
}

func TestFailuresMarkAbnormalAndSuccessRecovers(t *testing.T) {
	l, _ := newList(t)
	l.failTimes(A, FailureThreshold-1)
	if l.IsAbnormal(A) {
		t.Fatal("abnormal too early")
	}
	l.failTimes(A, 1)
	if !l.IsAbnormal(A) {
		t.Fatal("not abnormal")
	}
	l.RecordSuccess(A)
	if l.IsAbnormal(A) {
		t.Fatal("did not recover")
	}
	c, order := counts(A, 1)
	health := l.Entries(c, order)[0]["health"].(map[string]any)
	if health["failures"] != FailureThreshold || health["successes"] != 1 || health["last_error"] != "boom" {
		t.Fatalf("health %v", health)
	}
}

func TestReserveFillsLeastLoadedCountingExistingAccounts(t *testing.T) {
	l, _ := newList(t)
	got := mustReserve(t, l, []string{A, B, C}, 5, func() map[string]int { return map[string]int{A: 2, B: 0} })
	if !reflect.DeepEqual(got, []string{B, C, B, C, A}) {
		t.Fatalf("got %v", got)
	}
}

func TestReserveSkipsAbnormalUnlessAllAre(t *testing.T) {
	l, _ := newList(t)
	l.failTimes(B, FailureThreshold)
	if got := mustReserve(t, l, []string{A, B}, 3, none); !reflect.DeepEqual(got, []string{A, A, A}) {
		t.Fatalf("got %v", got)
	}
	l.Release([]string{A, A, A})
	l.failTimes(A, FailureThreshold)
	if got := mustReserve(t, l, []string{A, B}, 2, none); !reflect.DeepEqual(got, []string{A, B}) {
		t.Fatalf("got %v", got)
	}
}

func TestReserveRequiresACandidate(t *testing.T) {
	l, _ := newList(t)
	if _, err := l.Reserve([]string{"", " "}, 1, none, 0); err == nil {
		t.Fatal("empty candidates accepted")
	}
}

func TestReservationsCountUntilReleased(t *testing.T) {
	l, _ := newList(t)
	for i := 0; i < 2; i++ {
		if got := mustReserve(t, l, []string{A, B}, 2, none); !reflect.DeepEqual(got, []string{A, B}) {
			t.Fatalf("round %d: %v", i, got)
		}
	}
	l.Release([]string{A, A})
	if got := mustReserve(t, l, []string{A, B}, 2, none); !reflect.DeepEqual(got, []string{A, A}) {
		t.Fatalf("got %v", got)
	}
	l.Release([]string{A, A, B, B, B}) // releasing too many never goes negative
	if len(l.ReservedCounts()) != 0 {
		t.Fatalf("reserved %v", l.ReservedCounts())
	}
}

func TestSpellingsOfOneProxyShareEntryHealthAndLoad(t *testing.T) {
	l, _ := newList(t)
	l.Add("socks5://3.3.3.3:1080", "")
	if err := l.Add("SOCKS5H://3.3.3.3:1080/", ""); err == nil || !strings.Contains(err.Error(), "已经在清单里") {
		t.Fatalf("other spelling: %v", err)
	}
	l.failTimes("socks5h://3.3.3.3:1080", FailureThreshold)
	c, order := counts("socks5://3.3.3.3:1080", 1, "socks5h://3.3.3.3:1080", 2)
	items := l.Entries(c, order)
	if len(items) != 1 || items[0]["url"] != C || items[0]["account_count"] != 3 || !items[0]["health"].(map[string]any)["abnormal"].(bool) {
		t.Fatalf("items %v", items)
	}
	if err := l.Delete("socks5h://3.3.3.3:1080", map[string]int{C: 1, "socks5h://3.3.3.3:1080": 2}); err == nil || !strings.Contains(err.Error(), "3 个账号") {
		t.Fatalf("delete: %v", err)
	}
	if got := mustReserve(t, l, []string{"1.1.1.1:8000", "http://1.1.1.1:8000"}, 1, none); !reflect.DeepEqual(got, []string{"1.1.1.1:8000"}) {
		t.Fatalf("got %v", got)
	}
}

// test_proxy_reassign.py DistributionCapTests.test_pick_least_loaded_is_all_or_nothing
func TestPickLeastLoadedIsAllOrNothing(t *testing.T) {
	got, err := PickLeastLoaded([]string{"a", "b"}, map[string]int{"a": 1}, 3, 2)
	if err != nil || !reflect.DeepEqual(got, []string{"b", "a", "b"}) {
		t.Fatalf("got %v %v", got, err)
	}
	_, err = PickLeastLoaded([]string{"a", "b"}, map[string]int{"a": 1}, 4, 2)
	capacity, ok := err.(*CapacityError)
	if !ok || capacity.Shortfall != 1 || !strings.Contains(err.Error(), "还差 1 个位置") {
		t.Fatalf("err %v", err)
	}
}

func TestParseProxyLine(t *testing.T) {
	ok := map[string]string{
		"194.39.33.194:5903:rgqflcyp:gqnfwz1z3697": "http://rgqflcyp:gqnfwz1z3697@194.39.33.194:5903",
		"5.6.7.8:3128": "http://5.6.7.8:3128", "socks5://u:p@9.9.9.9:1080": "socks5://u:p@9.9.9.9:1080",
		"4.4.4.4:80:u:p@x": "http://u:p%40x@4.4.4.4:80",
	}
	for in, want := range ok {
		if got, err := ParseProxyLine(in); err != nil || got != want {
			t.Errorf("%s: got %s %v", in, got, err)
		}
	}
	bad := map[string]string{"not-a-proxy": "格式应为", "5.5.5.5:99999": "缺少主机或端口不正确", "FTP://6.6.6.6:21": "不支持的代理协议 ftp", "http://host": "缺少主机或端口不正确"}
	for in, want := range bad {
		if _, err := ParseProxyLine(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", in, err)
		}
	}
}
