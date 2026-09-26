package main

// RQ-WA-CLASSES-01 (2026-09-26) — bridge-side message-class policy.
//
// Every /api/send must carry a `class` (decide | incident | money_in |
// session_summary) and, for all but session_summary, a `key`. The bridge —
// not the calling script — decides whether the message goes out. This closes
// the 2026-04 → 2026-09 drift in which scripts self-declared `--priority P0`
// (or used one of three bypass priorities) and 14 scripts posted straight to
// :8082, so a "P0-only" policy delivered ~46 messages/day, ~2% of them P0.
//
// Rules (see configs/whatsapp_policy.json → "bridge"):
//   decide          one send per key, one reminder after N hours, then refused
//   incident        key is <id>:open or <id>:resolved; each key sends once, ever
//   money_in        one send per key, ever
//   session_summary no key dedup; daily cap only
//   quiet hours     decide + session_summary sent 23:00–07:00 ET are HELD and
//                   flushed as one batched message after 07:00 ET. The caller
//                   receives HTTP 202 {"success":true,"deferred":true} so it
//                   never retries.
//   daily caps      per class, America/New_York day
//
// Env:
//   WA_POLICY_ENFORCE=0   shadow mode — evaluate + ledger every decision, but send
//   WA_POLICY_CONFIG      path to whatsapp_policy.json (caps / quiet hours)
//   WA_POLICY_STATE       state file (default store/policy_state.json)
//   WA_POLICY_DEFERRED    held-message queue (default store/policy_deferred.jsonl)
//   WA_POLICY_LEDGER      append-only decision ledger
//                         (default /home/craigmbrown/Project/data/whatsapp_policy.jsonl)
//
// Fail-open where a policy bug would otherwise silence a real alert: an
// unreadable config falls back to the defaults below; an unreadable state
// file starts empty; a ledger write error never blocks a send.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

type policyConfig struct {
	Enforce             bool
	DailyCaps           map[string]int
	QuietStart          int // hour, ET
	QuietEnd            int // hour, ET
	QuietClasses        map[string]bool
	DecideReminderHours float64
	DecideMaxSends      int
}

type keyRec struct {
	Count int   `json:"count"`
	First int64 `json:"first"`
	Last  int64 `json:"last"`
}

type policyState struct {
	Keys  map[string]*keyRec `json:"keys"`
	Daily map[string]int     `json:"daily"`
}

type policyDecision struct {
	Allow    bool
	Deferred bool
	Reason   string
}

type deferredRow struct {
	TS        string `json:"ts"`
	Recipient string `json:"recipient"`
	Class     string `json:"class"`
	Key       string `json:"key"`
	Source    string `json:"source"`
	Message   string `json:"message"`
}

var (
	policyMu     sync.Mutex
	policyCfg    policyConfig
	policyLoaded bool
	policyET     *time.Location
)

var policyClasses = map[string]bool{
	"decide": true, "incident": true, "money_in": true, "session_summary": true,
}

func policyEnvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func policyStatePath() string {
	return policyEnvOr("WA_POLICY_STATE", "store/policy_state.json")
}
func policyDeferredPath() string {
	return policyEnvOr("WA_POLICY_DEFERRED", "store/policy_deferred.jsonl")
}
func policyLedgerPath() string {
	return policyEnvOr("WA_POLICY_LEDGER", "/home/craigmbrown/Project/data/whatsapp_policy.jsonl")
}

func defaultPolicyConfig() policyConfig {
	return policyConfig{
		Enforce:             os.Getenv("WA_POLICY_ENFORCE") != "0",
		DailyCaps:           map[string]int{"decide": 8, "incident": 20, "money_in": 30, "session_summary": 6},
		QuietStart:          23,
		QuietEnd:            7,
		QuietClasses:        map[string]bool{"decide": true, "session_summary": true},
		DecideReminderHours: 20,
		DecideMaxSends:      2,
	}
}

// loadPolicyConfig reads the "bridge" block of whatsapp_policy.json. Any
// problem yields the defaults (fail-open to a known-good policy, never to
// "no policy").
func loadPolicyConfig() policyConfig {
	cfg := defaultPolicyConfig()
	path := policyEnvOr("WA_POLICY_CONFIG", "/home/craigmbrown/Project/configs/whatsapp_policy.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var doc struct {
		Bridge struct {
			DailyCaps           map[string]int `json:"daily_caps"`
			QuietHoursET        []int          `json:"quiet_hours_et"`
			QuietClasses        []string       `json:"quiet_classes"`
			DecideReminderHours float64        `json:"decide_reminder_hours"`
			DecideMaxSends      int            `json:"decide_max_sends"`
		} `json:"bridge"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return cfg
	}
	b := doc.Bridge
	if len(b.DailyCaps) > 0 {
		for k, v := range b.DailyCaps {
			if v > 0 {
				cfg.DailyCaps[k] = v
			}
		}
	}
	if len(b.QuietHoursET) == 2 {
		cfg.QuietStart, cfg.QuietEnd = b.QuietHoursET[0], b.QuietHoursET[1]
	}
	if len(b.QuietClasses) > 0 {
		cfg.QuietClasses = map[string]bool{}
		for _, c := range b.QuietClasses {
			cfg.QuietClasses[c] = true
		}
	}
	if b.DecideReminderHours > 0 {
		cfg.DecideReminderHours = b.DecideReminderHours
	}
	if b.DecideMaxSends > 0 {
		cfg.DecideMaxSends = b.DecideMaxSends
	}
	return cfg
}

func policyLocation() *time.Location {
	if policyET != nil {
		return policyET
	}
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.FixedZone("EDT-fallback", -4*3600)
	}
	policyET = loc
	return loc
}

func ensurePolicyLoaded() {
	if !policyLoaded {
		policyCfg = loadPolicyConfig()
		policyLoaded = true
	}
}

func loadPolicyState() *policyState {
	st := &policyState{Keys: map[string]*keyRec{}, Daily: map[string]int{}}
	raw, err := os.ReadFile(policyStatePath())
	if err != nil {
		return st
	}
	_ = json.Unmarshal(raw, st)
	if st.Keys == nil {
		st.Keys = map[string]*keyRec{}
	}
	if st.Daily == nil {
		st.Daily = map[string]int{}
	}
	return st
}

func savePolicyState(st *policyState) {
	// keep the daily map from growing forever: drop anything older than 3 days
	today := time.Now().In(policyLocation())
	for k := range st.Daily {
		if len(k) >= 10 {
			if d, err := time.ParseInLocation("2006-01-02", k[:10], policyLocation()); err == nil {
				if today.Sub(d) > 72*time.Hour {
					delete(st.Daily, k)
				}
			}
		}
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(dirOf(policyStatePath()), 0o755)
	_ = os.WriteFile(policyStatePath(), raw, 0o644)
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}

func policyLedger(row map[string]interface{}) {
	row["ts"] = time.Now().UTC().Format(time.RFC3339)
	raw, err := json.Marshal(row)
	if err != nil {
		return
	}
	_ = os.MkdirAll(dirOf(policyLedgerPath()), 0o755)
	f, err := os.OpenFile(policyLedgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

func (c policyConfig) inQuietHours(t time.Time) bool {
	h := t.In(policyLocation()).Hour()
	if c.QuietStart == c.QuietEnd {
		return false
	}
	if c.QuietStart > c.QuietEnd { // wraps midnight, e.g. 23 → 7
		return h >= c.QuietStart || h < c.QuietEnd
	}
	return h >= c.QuietStart && h < c.QuietEnd
}

func preview(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 90 {
		return s[:90]
	}
	return s
}

// evaluatePolicy is the pure decision. It mutates st (counts the send or the
// deferral) only when the decision is Allow or Deferred.
func evaluatePolicy(cfg policyConfig, st *policyState, class, key string, now time.Time) policyDecision {
	class = strings.ToLower(strings.TrimSpace(class))
	key = strings.TrimSpace(key)
	if class == "" {
		return policyDecision{Reason: "class_required"}
	}
	if !policyClasses[class] {
		return policyDecision{Reason: "class_unknown:" + class}
	}
	if class != "session_summary" && key == "" {
		return policyDecision{Reason: "key_required"}
	}

	day := now.In(policyLocation()).Format("2006-01-02")
	dayKey := day + ":" + class
	if cap, ok := cfg.DailyCaps[class]; ok && st.Daily[dayKey] >= cap {
		return policyDecision{Reason: fmt.Sprintf("daily_cap_%s_%d", class, cap)}
	}

	skey := class + ":" + key
	rec := st.Keys[skey]
	switch class {
	case "decide":
		if rec != nil {
			if rec.Count >= cfg.DecideMaxSends {
				return policyDecision{Reason: "decide_exhausted_lives_in_brief"}
			}
			since := now.Sub(time.Unix(rec.First, 0)).Hours()
			if since < cfg.DecideReminderHours {
				return policyDecision{Reason: fmt.Sprintf("decide_reminder_too_soon_%.0fh", cfg.DecideReminderHours-since)}
			}
		}
	case "incident":
		if !(strings.HasSuffix(key, ":open") || strings.HasSuffix(key, ":resolved")) {
			return policyDecision{Reason: "incident_key_needs_open_or_resolved_suffix"}
		}
		if rec != nil {
			return policyDecision{Reason: "incident_key_already_sent"}
		}
	case "money_in":
		if rec != nil {
			return policyDecision{Reason: "money_in_duplicate"}
		}
	}

	// count it (a deferral is a send from the dedup's point of view)
	if class != "session_summary" {
		if rec == nil {
			rec = &keyRec{First: now.Unix()}
			st.Keys[skey] = rec
		}
		rec.Count++
		rec.Last = now.Unix()
	}
	st.Daily[dayKey]++

	if cfg.QuietClasses[class] && cfg.inQuietHours(now) {
		return policyDecision{Allow: true, Deferred: true, Reason: "quiet_hours_held"}
	}
	return policyDecision{Allow: true, Reason: "ok"}
}

// policyCheck is the handler-facing entry point: loads state, evaluates,
// persists, ledgers. Returns the decision the HTTP layer should act on.
func policyCheck(req SendMessageRequest) policyDecision {
	policyMu.Lock()
	defer policyMu.Unlock()
	ensurePolicyLoaded()
	st := loadPolicyState()
	now := time.Now()
	d := evaluatePolicy(policyCfg, st, req.Class, req.Key, now)
	if d.Allow {
		savePolicyState(st)
	}
	row := map[string]interface{}{
		"class": req.Class, "key": req.Key, "source": req.Source,
		"recipient": req.Recipient, "preview": preview(req.Message),
		"reason": d.Reason, "enforce": policyCfg.Enforce,
	}
	switch {
	case d.Allow && d.Deferred:
		row["decision"] = "deferred"
	case d.Allow:
		row["decision"] = "allow"
	case !policyCfg.Enforce:
		row["decision"] = "shadow_would_refuse"
		d = policyDecision{Allow: true, Reason: "shadow:" + d.Reason}
	default:
		row["decision"] = "refuse"
	}
	policyLedger(row)
	return d
}

func enqueueDeferred(req SendMessageRequest) {
	policyMu.Lock()
	defer policyMu.Unlock()
	row := deferredRow{
		TS: time.Now().UTC().Format(time.RFC3339), Recipient: req.Recipient,
		Class: req.Class, Key: req.Key, Source: req.Source, Message: req.Message,
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return
	}
	_ = os.MkdirAll(dirOf(policyDeferredPath()), 0o755)
	f, err := os.OpenFile(policyDeferredPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// drainDeferred returns the held rows grouped by recipient and truncates the
// queue. Called only outside quiet hours.
func drainDeferred() map[string][]deferredRow {
	policyMu.Lock()
	defer policyMu.Unlock()
	raw, err := os.ReadFile(policyDeferredPath())
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	out := map[string][]deferredRow{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r deferredRow
		if json.Unmarshal([]byte(line), &r) == nil && r.Recipient != "" {
			out[r.Recipient] = append(out[r.Recipient], r)
		}
	}
	_ = os.WriteFile(policyDeferredPath(), []byte{}, 0o644)
	return out
}

func composeDeferredBatch(rows []deferredRow) string {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TS < rows[j].TS })
	var b strings.Builder
	fmt.Fprintf(&b, "⏰ Held overnight — %d message(s)\n", len(rows))
	for i, r := range rows {
		msg := r.Message
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		when := r.TS
		if t, err := time.Parse(time.RFC3339, r.TS); err == nil {
			when = t.In(policyLocation()).Format("Mon 15:04")
		}
		fmt.Fprintf(&b, "\n%d) [%s · %s]\n%s\n", i+1, when, r.Source, msg)
	}
	return b.String()
}

// startDeferredFlusher sends held messages once quiet hours end. One batched
// message per recipient per flush.
func startDeferredFlusher(send func(recipient, message string) (bool, string)) {
	go func() {
		for {
			time.Sleep(60 * time.Second)
			ensurePolicyLoaded()
			if policyCfg.inQuietHours(time.Now()) {
				continue
			}
			groups := drainDeferred()
			for rcpt, rows := range groups {
				if len(rows) == 0 {
					continue
				}
				ok, msg := send(rcpt, composeDeferredBatch(rows))
				policyLedger(map[string]interface{}{
					"decision": "flushed", "recipient": rcpt, "count": len(rows),
					"ok": ok, "detail": msg,
				})
			}
		}
	}()
}
