package telemetry

import "regexp"

// Redaction patterns for captured prompts and completions. They catch common
// secrets and personal data; they are a safety net, not a guarantee, which is
// why capture is off unless a tenant opts in.
var redactions = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\b(sk-ant-[a-z0-9_-]{10,}|sk-[a-z0-9_-]{16,}|sy_[a-z0-9]{8}_[a-z0-9]{20,}|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{30,})\b`), "[REDACTED_KEY]"},
	{regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{16,}`), "Bearer [REDACTED_TOKEN]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[REDACTED_EMAIL]"},
	{regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), "[REDACTED_NUMBER]"},
	{regexp.MustCompile(`\+?\(?\d{1,3}\)?[ .-]?\(?\d{2,4}\)?[ .-]?\d{3}[ .-]?\d{2,4}(?:[ .-]?\d{2})?\b`), "[REDACTED_PHONE]"},
}

// Redact masks secrets, emails, card-like numbers and phone numbers.
func Redact(s string) string {
	for _, r := range redactions {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
