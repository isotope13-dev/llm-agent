package llmagent

import "testing"

func TestDetectRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		ok                bool
	}{
		{
			name: "claude refusal system event",
			input: `{"type":"system","subtype":"model_refusal_no_fallback","api_refusal_category":"cyber",` +
				`"api_refusal_explanation":"This request triggered cyber-related safeguards."}`,
			want: "This request triggered cyber-related safeguards.", ok: true,
		},
		{
			name: "assistant stop details",
			input: `{"type":"assistant","message":{"stop_reason":"refusal",` +
				`"stop_details":{"type":"refusal","category":"cyber","explanation":"Flagged."}}}`,
			want: "Flagged.", ok: true,
		},
		{
			name:  "bare result",
			input: `{"type":"result","stop_reason":"refusal","total_cost_usd":9.73}`,
			ok:    true,
		},
		{
			// A transcript about malware says "refusal" in prose all the time.
			name:  "prose is not a signal",
			input: `{"type":"assistant","message":{"content":[{"type":"text","text":"the refusal path in this loader"}]}}`,
		},
		{name: "ordinary failure", input: "connection refused"},
	} {
		got, ok := DetectRefusal(tc.input)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: DetectRefusal = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// claude reports a rate_limit_event on every run. One that says "allowed" is
// not quota, which is how a refused run was benched for two hours on
// 2026-11-07; one that says "rejected" still is.
func TestDetectQuotaIgnoresAllowedRateLimitEvent(t *testing.T) {
	allowed := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1794452400,` +
		`"rateLimitType":"seven_day","unifiedWindows":{"seven_day":{"utilization":0.53,"resetsAt":1794452400,"status":"allowed"}}}}` +
		"\n" + `{"type":"result","is_error":true,"result":"exit status 1"}`
	if d, ok := DetectQuota(allowed); ok {
		t.Errorf("allowed rate_limit_event read as quota (%q)", d)
	}
	rejected := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1794452400}}`
	if _, ok := DetectQuota(rejected); !ok {
		t.Error("rejected rate_limit_event not read as quota")
	}
	// The refusal itself, beside an allowed event, is neither quota nor auth
	// nor transient.
	refused := allowed + "\n" + `{"type":"system","subtype":"model_refusal_no_fallback","api_refusal_explanation":"x"}`
	if _, ok := DetectQuota(refused); ok {
		t.Error("refusal read as quota")
	}
	if _, ok := DetectAuth(refused); ok {
		t.Error("refusal read as auth")
	}
	if DetectTransient(refused) {
		t.Error("refusal read as transient")
	}
}

// 429 and 402 count as status codes, not as digits inside an id.
func TestDetectQuotaStatusCodesNotIDs(t *testing.T) {
	for input, want := range map[string]bool{
		"API error 429 [request_id=x]: slow down":                                true,
		`{"error":{"status":429}}`:                                               true,
		"HTTP 402 Payment":                                                       true,
		`{"uuid":"083b57a7-a3dd-41a7-b837-393a5fab2402","type":"result"}`:        false,
		`{"session_id":"01a1b628-4290-7f43-a886-730a5c863384"}`:                  false,
		`{"duration_ms":14290,"result":"Failed to authenticate: OAuth expired"}`: false,
	} {
		if _, got := DetectQuota(input); got != want {
			t.Errorf("DetectQuota(%q) = %v, want %v", input, got, want)
		}
	}
}
