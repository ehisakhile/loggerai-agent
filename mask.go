package main

import "regexp"

// Secrets take these shapes in logs. The API masks again when it stores them, but the first copy
// of this list is here, so what's caught never leaves the machine.
var masks = []struct {
	re *regexp.Regexp
	to string
}{
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "[REDACTED_JWT]"},
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9\-._~+/]{12,}=*`), "$1 [REDACTED]"},
	{regexp.MustCompile(`\b(sk|pk|rk)_(live|test)_[A-Za-z0-9]{8,}`), "[REDACTED_KEY]"},
	{regexp.MustCompile(`\b(AKIA|ASIA)[A-Z0-9]{16}\b`), "[REDACTED_KEY]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`), "[REDACTED_KEY]"},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), "[REDACTED_KEY]"},
	{regexp.MustCompile(`([a-z][a-z0-9+.-]*://[^\s:/@]+:)[^\s@/]+@`), "$1[REDACTED]@"},
	{regexp.MustCompile(`(?i)\b((?:[a-z0-9_-]*)(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)["']?\s*[=:]\s*["']?)[^\s"',&;}]+`), "$1[REDACTED]"},
	{regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`), "[REDACTED_NUMBER]"},
}

func mask(s string) string {
	for _, m := range masks {
		s = m.re.ReplaceAllString(s, m.to)
	}
	return s
}
