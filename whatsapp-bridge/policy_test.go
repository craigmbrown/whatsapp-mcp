package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func testCfg() policyConfig {
	c := defaultPolicyConfig()
	c.Enforce = true
	return c
}

func newState() *policyState {
	return &policyState{Keys: map[string]*keyRec{}, Daily: map[string]int{}}
}

func noon() time.Time { // 12:00 ET, outside quiet hours
	return time.Date(2026, 9, 26, 12, 0, 0, 0, policyLocation())
}

func TestClassRequired(t *testing.T) {
	d := evaluatePolicy(testCfg(), newState(), "", "k", noon())
	if d.Allow || d.Reason != "class_required" {
		t.Fatalf("got %+v", d)
	}
	d = evaluatePolicy(testCfg(), newState(), "P0", "k", noon())
	if d.Allow {
		t.Fatalf("legacy priority must not be a class: %+v", d)
	}
}

func TestKeyRequiredExceptSessionSummary(t *testing.T) {
	if d := evaluatePolicy(testCfg(), newState(), "decide", "", noon()); d.Allow {
		t.Fatalf("decide without key allowed: %+v", d)
	}
	if d := evaluatePolicy(testCfg(), newState(), "session_summary", "", noon()); !d.Allow {
		t.Fatalf("session_summary needs no key: %+v", d)
	}
}

func TestDecideOnceThenReminderThenBrief(t *testing.T) {
	st := newState()
	c := testCfg()
	t0 := noon()
	if d := evaluatePolicy(c, st, "decide", "plan-1", t0); !d.Allow {
		t.Fatalf("first: %+v", d)
	}
	if d := evaluatePolicy(c, st, "decide", "plan-1", t0.Add(2*time.Hour)); d.Allow {
		t.Fatalf("hourly re-ask must be refused: %+v", d)
	}
	if d := evaluatePolicy(c, st, "decide", "plan-1", t0.Add(21*time.Hour)); !d.Allow {
		t.Fatalf("one reminder after 20h: %+v", d)
	}
	if d := evaluatePolicy(c, st, "decide", "plan-1", t0.Add(50*time.Hour)); d.Allow || d.Reason != "decide_exhausted_lives_in_brief" {
		t.Fatalf("third ask must go to the brief: %+v", d)
	}
}

func TestIncidentOpenResolvedOnce(t *testing.T) {
	st := newState()
	c := testCfg()
	if d := evaluatePolicy(c, st, "incident", "bridge-down", noon()); d.Allow {
		t.Fatalf("incident key needs suffix: %+v", d)
	}
	if d := evaluatePolicy(c, st, "incident", "bridge-down:open", noon()); !d.Allow {
		t.Fatalf("open: %+v", d)
	}
	if d := evaluatePolicy(c, st, "incident", "bridge-down:open", noon().Add(time.Hour)); d.Allow {
		t.Fatalf("open repeated: %+v", d)
	}
	if d := evaluatePolicy(c, st, "incident", "bridge-down:resolved", noon().Add(2*time.Hour)); !d.Allow {
		t.Fatalf("resolved: %+v", d)
	}
}

func TestMoneyInDedupForever(t *testing.T) {
	st := newState()
	c := testCfg()
	if d := evaluatePolicy(c, st, "money_in", "0xabc", noon()); !d.Allow {
		t.Fatalf("first: %+v", d)
	}
	if d := evaluatePolicy(c, st, "money_in", "0xabc", noon().Add(30*24*time.Hour)); d.Allow {
		t.Fatalf("the 57x 'FIRST SALE' repeat: %+v", d)
	}
}

func TestDailyCap(t *testing.T) {
	st := newState()
	c := testCfg()
	c.DailyCaps["session_summary"] = 2
	for i := 0; i < 2; i++ {
		if d := evaluatePolicy(c, st, "session_summary", "", noon()); !d.Allow {
			t.Fatalf("send %d: %+v", i, d)
		}
	}
	if d := evaluatePolicy(c, st, "session_summary", "", noon()); d.Allow {
		t.Fatalf("cap not applied: %+v", d)
	}
}

func TestQuietHoursHoldDecideNotIncident(t *testing.T) {
	st := newState()
	c := testCfg()
	night := time.Date(2026, 9, 27, 2, 30, 0, 0, policyLocation())
	d := evaluatePolicy(c, st, "decide", "plan-2", night)
	if !d.Allow || !d.Deferred {
		t.Fatalf("decide at 02:30 ET must be held: %+v", d)
	}
	// the hold counts as the send: a retry at 03:00 is refused, not re-held
	if d2 := evaluatePolicy(c, st, "decide", "plan-2", night.Add(30*time.Minute)); d2.Allow {
		t.Fatalf("retry during hold must be refused: %+v", d2)
	}
	if d3 := evaluatePolicy(c, st, "incident", "x:open", night); !d3.Allow || d3.Deferred {
		t.Fatalf("incident must pass at night: %+v", d3)
	}
	if d4 := evaluatePolicy(c, st, "money_in", "tx1", night); !d4.Allow || d4.Deferred {
		t.Fatalf("money_in must pass at night: %+v", d4)
	}
}

func TestQuietWindowBoundaries(t *testing.T) {
	c := testCfg()
	cases := map[int]bool{22: false, 23: true, 0: true, 6: true, 7: false, 12: false}
	for h, want := range cases {
		tm := time.Date(2026, 9, 26, h, 15, 0, 0, policyLocation())
		if got := c.inQuietHours(tm); got != want {
			t.Fatalf("hour %d: got %v want %v", h, got, want)
		}
	}
}

func TestStateRoundTripAndLedgerFailOpen(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("WA_POLICY_STATE", dir+"/state.json")
	os.Setenv("WA_POLICY_LEDGER", dir+"/ledger.jsonl")
	os.Setenv("WA_POLICY_DEFERRED", dir+"/deferred.jsonl")
	defer os.Unsetenv("WA_POLICY_STATE")
	defer os.Unsetenv("WA_POLICY_LEDGER")
	defer os.Unsetenv("WA_POLICY_DEFERRED")
	policyLoaded = false
	policyCfg = testCfg()
	policyLoaded = true
	req := SendMessageRequest{Recipient: "1", Message: "hi", Class: "money_in", Key: "tx9", Source: "t"}
	if d := policyCheck(req); !d.Allow {
		t.Fatalf("first: %+v", d)
	}
	if d := policyCheck(req); d.Allow {
		t.Fatalf("state must persist across calls: %+v", d)
	}
	raw, _ := os.ReadFile(dir + "/ledger.jsonl")
	if len(raw) == 0 {
		t.Fatalf("ledger empty")
	}
	policyLoaded = false
}

func TestDeferredBatchComposition(t *testing.T) {
	rows := []deferredRow{
		{TS: "2026-09-27T05:00:00Z", Recipient: "1", Source: "tb-scout-digest", Message: "🍼 TB Scout — 3 plans awaiting you"},
		{TS: "2026-09-27T04:00:00Z", Recipient: "1", Source: "nostr-gtm", Message: "🛰️ Nostr GTM review"},
	}
	s := composeDeferredBatch(rows)
	if !strings.HasPrefix(s, "⏰ Held overnight — 2") {
		t.Fatalf("bad batch: %q", s)
	}
	if idx := indexOf(s, "nostr-gtm"); idx < 0 || idx > indexOf(s, "tb-scout-digest") {
		t.Fatalf("batch must be chronological: %q", s)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
