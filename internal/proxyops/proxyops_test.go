package proxyops

import "testing"

// test_proxy_reassign.py CheckTests.test_all_402_failures_are_a_provider_outage
func TestAll402FailuresAreAProviderOutage(t *testing.T) {
	failures := func(n int, cause string) []map[string]any {
		out := []map[string]any{}
		for i := 0; i < n; i++ {
			out = append(out, map[string]any{"error": cause})
		}
		return out
	}
	if !IsProviderOutage(10, failures(3, "CONNECT tunnel failed, response 402")) {
		t.Fatal("3 x 402 of 10 should be an outage")
	}
	if IsProviderOutage(10, failures(2, "CONNECT tunnel failed, response 402")) {
		t.Fatal("2 x 402 is not enough")
	}
	if IsProviderOutage(10, failures(3, "timed out")) {
		t.Fatal("3 timeouts of 10 are not an outage")
	}
	if IsProviderOutage(1, failures(1, "x")) {
		t.Fatal("one proxy tested is never an outage")
	}
	if !IsProviderOutage(4, failures(3, "x")) {
		t.Fatal("more than half failing is an outage")
	}
}
