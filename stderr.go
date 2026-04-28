package llmagent

import "strings"

// stderrNoise are substrings that mark provider-startup banners and other
// non-actionable stderr lines, suppressed from OnStderr callbacks.
var stderrNoise = []string{
	"YOLO mode",
	"Loaded cached credentials",
	"credentials for project",
	"Welcome to",
}

// IsStderrNoise reports whether line matches a known noise pattern.
func IsStderrNoise(line string) bool {
	for _, pat := range stderrNoise {
		if strings.Contains(line, pat) {
			return true
		}
	}
	return false
}
