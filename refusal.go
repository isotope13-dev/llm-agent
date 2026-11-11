package llmagent

import (
	"encoding/json"
	"strings"
)

// DetectRefusal reports whether output ends a run the model refused, and if so
// returns the provider's explanation.
//
// A refusal is a verdict on the prompt, not on the provider: the binary ran,
// the credential authenticated, the backend answered, and the same provider
// will take a different prompt at once. It has to be told apart from quota,
// auth and transient failures because the run's output is a whole transcript,
// and a transcript matches their substring vocabularies by accident. On
// 2026-11-07 a cyber-safeguard refusal from claude was benched for two hours
// as quota, on the strength of a rate_limit_event that said "allowed".
//
// Only structured signals count, never refusal-sounding prose, which a
// transcript about malware is full of. Every entry is a shape claude emitted:
//
//	{"type":"system","subtype":"model_refusal_no_fallback",
//	 "api_refusal_category":"cyber","api_refusal_explanation":"This request ..."}
//	{"type":"assistant","message":{"stop_reason":"refusal",
//	 "stop_details":{"type":"refusal","explanation":"This request ..."}}}
//	{"type":"result","stop_reason":"refusal", ...}
func DetectRefusal(output string) (detail string, ok bool) {
	if !strings.Contains(output, "refusal") {
		return "", false
	}
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.Contains(line, "refusal") {
			continue
		}
		var ev struct {
			Subtype     string `json:"subtype"`
			Explanation string `json:"api_refusal_explanation"`
			StopReason  string `json:"stop_reason"`
			Message     struct {
				StopReason  string `json:"stop_reason"`
				StopDetails struct {
					Type        string `json:"type"`
					Explanation string `json:"explanation"`
				} `json:"stop_details"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch {
		case strings.HasPrefix(ev.Subtype, "model_refusal"):
			detail = ev.Explanation
		case ev.Message.StopDetails.Type == "refusal":
			detail = ev.Message.StopDetails.Explanation
		case ev.StopReason == "refusal" || ev.Message.StopReason == "refusal":
		default:
			continue
		}
		ok = true
		if detail != "" {
			return detail, true
		}
	}
	return "", ok
}

// rateLimitEventMarker opens claude's per-run rate-limit report. claude emits
// one on every run, and nearly all of them say "allowed".
const rateLimitEventMarker = `{"type":"rate_limit_event"`

// withoutAllowedRateLimitEvents removes the rate_limit_events that do not
// report a rejection, so that "rate_limit" in their type and the resetsAt of a
// window with room left are not read as quota. A clipped event that no longer
// parses is kept: losing a real rejection costs a hot retry loop, keeping a
// stale one costs one cooldown.
func withoutAllowedRateLimitEvents(output string) string {
	if !strings.Contains(output, rateLimitEventMarker) {
		return output
	}
	var b strings.Builder
	rest := output
	for {
		i := strings.Index(rest, rateLimitEventMarker)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i])
		dec := json.NewDecoder(strings.NewReader(rest[i:]))
		var ev struct {
			Info struct {
				Status string `json:"status"`
			} `json:"rate_limit_info"`
		}
		if dec.Decode(&ev) != nil {
			b.WriteString(rest[i:])
			return b.String()
		}
		n := int(dec.InputOffset())
		if ev.Info.Status == "rejected" {
			b.WriteString(rest[i : i+n])
		}
		rest = rest[i+n:]
	}
}
