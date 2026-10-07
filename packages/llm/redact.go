package llm

import (
	"regexp"
	"strings"
)

// Redacted replaces hidden secrets.
const Redacted = "[REDACTED]"

var secretPatterns = []*regexp.Regexp{
	// Provider keys and tokens with well-known prefixes.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`),
	regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), // JWT
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// Authorization headers and key=value pairs keep their name, lose the value.
var (
	bearerPattern  = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=\-]{8,}`)
	kvPattern      = regexp.MustCompile(`(?i)(\b(?:[a-z0-9_\-]*_)?(?:api[_\-]?key|apikey|access[_\-]?token|auth[_\-]?token|token|secret|client[_\-]?secret|password|passwd|pwd)|密码|密钥|令牌)(\s*[:=：]\s*|\s+)("[^"]*"|'[^']*'|[^\s,;&"']+)`)
	urlCredPattern = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/\s:@]+:[^/\s@]+@`)
)

// Redact hides API keys, tokens, passwords and the given literal secrets
// (FR-607). It is applied to everything that is logged, stored with a model
// session or sent to the model.
func Redact(s string, secrets ...string) string {
	if s == "" {
		return s
	}
	for _, sec := range secrets {
		if len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, Redacted)
		}
	}
	for _, p := range secretPatterns {
		s = p.ReplaceAllString(s, Redacted)
	}
	s = bearerPattern.ReplaceAllString(s, "$1 "+Redacted)
	s = urlCredPattern.ReplaceAllString(s, "${1}"+Redacted+"@")
	s = kvPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := kvPattern.FindStringSubmatch(m)
		if sub[3] == Redacted || strings.HasPrefix(sub[3], "[") {
			return m
		}
		// "token 5" or "password reset" are words, not secrets: only hide
		// values after a separator or values that look like credentials.
		if strings.TrimSpace(sub[2]) == "" && !looksSecret(sub[3]) {
			return m
		}
		return sub[1] + sub[2] + Redacted
	})
	return s
}

// looksSecret reports whether a bare word looks like a credential: long and
// mixing letters with digits or symbols.
func looksSecret(v string) bool {
	if len(v) < 12 {
		return false
	}
	var letters, others int
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			letters++
		case r >= '0' && r <= '9' || strings.ContainsRune("_-+/=.", r):
			others++
		default:
			return false
		}
	}
	return letters > 0 && others > 0
}
