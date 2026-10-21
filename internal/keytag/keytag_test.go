package keytag

import (
	"strings"
	"testing"
)

func TestSplitToken(t *testing.T) {
	cases := []struct {
		in     string
		base   string
		suffix string
		ok     bool
	}{
		{"sk-lm-abc", "sk-lm-abc", "", false},
		{"sk-lm-abc#cli", "sk-lm-abc", "cli", true},
		{"sk-lm-abc#cli&pool_policy=price", "sk-lm-abc", "cli&pool_policy=price", true},
		{"sk-lm-abc#", "sk-lm-abc", "", true},
		{"#cli", "#cli", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		base, suffix, ok := SplitToken(c.in)
		if base != c.base || suffix != c.suffix || ok != c.ok {
			t.Errorf("SplitToken(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, base, suffix, ok, c.base, c.suffix, c.ok)
		}
	}
}

func TestParseSuffix(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		tag   string
		hints map[string]string
	}{
		{"tag only", "claude-code", "claude-code", nil},
		{"tag and policy", "claude-code&pool_policy=Price_First", "claude-code", map[string]string{"pool_policy": "price_first"}},
		{"hints only", "pool_policy=price_first", "", map[string]string{"pool_policy": "price_first"}},
		{"extra bare ignored", "tag&ignored&pool_policy=price", "tag", map[string]string{"pool_policy": "price"}},
		{"decoded tag", "claude%2Dcode&x=a%26b", "claude-code", map[string]string{"x": "a&b"}},
		{"duplicate last wins", "tag&pool_policy=price&pool_policy=speed", "tag", map[string]string{"pool_policy": "speed"}},
		{"empty segments", "tag&&pool_policy=price&", "tag", map[string]string{"pool_policy": "price"}},
		{"invalid pieces dropped", "tag&bad key=v&x=ok", "tag", map[string]string{"x": "ok"}},
		{"too long suffix", strings.Repeat("x", MaxSuffix+1), "", nil},
	}
	for _, c := range cases {
		got := ParseSuffix(c.in)
		if got.Tag != c.tag {
			t.Errorf("%s: tag = %q, want %q", c.name, got.Tag, c.tag)
		}
		if len(got.Hints) != len(c.hints) {
			t.Errorf("%s: hints = %v, want %v", c.name, got.Hints, c.hints)
			continue
		}
		for k, v := range c.hints {
			if got.Hints[k] != v {
				t.Errorf("%s: hints[%q] = %q, want %q", c.name, k, got.Hints[k], v)
			}
		}
	}
}

func TestParseQueryIgnoresBare(t *testing.T) {
	got := ParseQuery("tag&pool_policy=price")
	if len(got) != 1 || got["pool_policy"] != "price" {
		t.Fatalf("ParseQuery = %v", got)
	}
}

func TestCanonical(t *testing.T) {
	if got := Canonical(map[string]string{"tier": "fast", "pool_policy": "price_first"}); got != "pool_policy=price_first&tier=fast" {
		t.Fatalf("Canonical = %q", got)
	}
	if got := Canonical(nil); got != "" {
		t.Fatalf("empty Canonical = %q", got)
	}
}

func TestNormalizeMap(t *testing.T) {
	got := NormalizeMap(map[string]any{
		"Pool_Policy": "Price_First",
		"bad":         "a b",
		"num":         1,
	})
	if len(got) != 1 || got["pool_policy"] != "price_first" {
		t.Fatalf("NormalizeMap = %v", got)
	}
}

func TestValidTag(t *testing.T) {
	cases := []struct {
		tag string
		ok  bool
	}{
		{"cli", true},
		{"claude-code", true},
		{"team.ops:v2", true},
		{"a&b", true}, // '&' is only a separator in the suffix; decoded tags may contain it
		{"客户端", true},
		{"", false},
		{"a b", false},
		{"a\tb", false},
		{"\x00", false},
		{strings.Repeat("x", MaxTagLen), true},
		{strings.Repeat("x", MaxTagLen+1), false},
	}
	for _, c := range cases {
		if got := ValidTag(c.tag); got != c.ok {
			t.Errorf("ValidTag(%q) = %v, want %v", c.tag, got, c.ok)
		}
	}
}
