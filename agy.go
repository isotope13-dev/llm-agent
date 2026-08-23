package llmagent

import "encoding/json"

// agy is Antigravity's CLI, the successor to the standalone gemini CLI. It
// serves the Gemini models (plus a few third-party ones) and speaks a print
// mode close to claude's: --output-format stream-json on the way out,
// --input-format stream-json on the way in.
//
// Unlike every other provider here, agy's --print flag takes the prompt as its
// value, which would put a whole prompt on the command line. --input-format
// stream-json keeps it on stdin instead: one NDJSON message per turn, one turn
// per Run, and the process exits when stdin closes.

// usesAgy reports whether provider runs through the agy binary. "gemini" is
// kept as an alias so existing provider lists keep resolving to the same
// vendor; its model names, however, are agy's (see `agy models`).
func usesAgy(provider string) bool {
	switch Base(provider) {
	case "agy", "gemini":
		return true
	}
	return false
}

// agyInput is one line of agy's stream-json input: a single user turn.
type agyInput struct {
	Event   string     `json:"event"`
	Message agyMessage `json:"message"`
}

type agyMessage struct {
	Role    string     `json:"role"`
	Content []agyBlock `json:"content"`
}

type agyBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// agyStreamInput encodes prompt as the single NDJSON line agy reads from
// stdin. Marshalling a struct of plain strings cannot fail.
func agyStreamInput(prompt string) string {
	line, err := json.Marshal(agyInput{
		Event: "user",
		Message: agyMessage{
			Role:    "user",
			Content: []agyBlock{{Type: "text", Text: prompt}},
		},
	})
	if err != nil {
		return ""
	}
	return string(line) + "\n"
}
