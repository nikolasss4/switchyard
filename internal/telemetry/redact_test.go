package telemetry

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	in := "mail bob.smith@example.com, key sk-ant-api03-abcdefghijklmnop, gw sy_abcd1234_0123456789abcdefghijABCDEFGHIJ0123 " +
		"card 4111 1111 1111 1111, call +380 67 123 45 67, auth Bearer eyJhbGciOiJIUzI1NiJ9.abc.def"
	out := Redact(in)
	for _, leak := range []string{"bob.smith", "sk-ant-api03", "sy_abcd1234", "4111 1111", "123 45 67", "eyJhbGci"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %q", leak, out)
		}
	}
	if keep := "the capital of France is Paris in 2026"; Redact(keep) != keep {
		t.Errorf("over-redacted: %q", Redact(keep))
	}
}
