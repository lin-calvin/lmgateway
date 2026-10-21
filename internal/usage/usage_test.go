package usage

import (
	"testing"
	"time"

	"lmgateway/internal/keytag"
	"lmgateway/internal/packet"
)

func TestCountsFromMapChat(t *testing.T) {
	c := CountsFromMap(map[string]any{
		"prompt_tokens":             float64(10),
		"completion_tokens":         float64(20),
		"total_tokens":              float64(30),
		"prompt_tokens_details":     map[string]any{"cached_tokens": float64(4)},
		"completion_tokens_details": map[string]any{"reasoning_tokens": float64(5)},
	})
	if c.Prompt != 10 || c.Completion != 20 || c.Total != 30 || c.Cached != 4 || c.Reasoning != 5 {
		t.Fatalf("chat counts wrong: %+v", c)
	}
	if !c.HasPrompt || !c.HasCompletion || !c.HasTotal {
		t.Fatalf("expected known flags: %+v", c)
	}
}

func TestCountsFromMapResponses(t *testing.T) {
	c := CountsFromMap(map[string]any{
		"input_tokens":          float64(100),
		"output_tokens":         float64(25),
		"total_tokens":          float64(125),
		"output_tokens_details": map[string]any{"reasoning_tokens": float64(10)},
		"input_tokens_details":  map[string]any{"cached_tokens": float64(80)},
	})
	if c.Prompt != 100 || c.Completion != 25 || c.Total != 125 || c.Reasoning != 10 || c.Cached != 80 {
		t.Fatalf("responses counts wrong: %+v", c)
	}
}

func TestExtractTiming(t *testing.T) {
	pkt := packet.NewReq(map[string]any{"model": "m", "stream": true})
	pkt.Set(packet.KeyResp, map[string]any{
		"model": "m",
		"usage": map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(7), "total_tokens": float64(10)},
	})
	pkt.Set(packet.KeyProvider, "prov")
	start := time.Now().Add(-100 * time.Millisecond)
	first := start.Add(40 * time.Millisecond)
	end := start.Add(90 * time.Millisecond)
	pkt.Set(packet.KeyStart, start)
	pkt.Set(packet.KeyFirst, first)
	pkt.Set(packet.KeyEnd, end)

	u, ok := Extract(pkt)
	if !ok {
		t.Fatal("expected usage")
	}
	if u.Model != "m" || u.Provider != "prov" || !u.Stream {
		t.Fatalf("metadata wrong: %+v", u)
	}
	if !u.HasStart || !u.HasTTFT {
		t.Fatalf("expected timing flags: %+v", u)
	}
	if u.TTFTMs < 35 || u.TTFTMs > 45 {
		t.Fatalf("ttft = %v, want ~40", u.TTFTMs)
	}
	if u.GenerationMs < 45 || u.GenerationMs > 55 {
		t.Fatalf("generation = %v, want ~50", u.GenerationMs)
	}
	if u.GenerationTokens != 7 {
		t.Fatalf("generation tokens = %v, want 7", u.GenerationTokens)
	}
}

func TestExtractKeyTag(t *testing.T) {
	pkt := packet.NewReq(map[string]any{"model": "m", keytag.TagField: "cli"})
	pkt.Set(packet.KeyResp, map[string]any{
		"model": "m",
		"usage": map[string]any{"prompt_tokens": 1.0},
	})
	u, ok := Extract(pkt)
	if !ok || u.KeyTag != "cli" {
		t.Fatalf("key tag = %q, ok=%v", u.KeyTag, ok)
	}
}

func TestExtractNoResponse(t *testing.T) {
	if _, ok := Extract(packet.NewReq(map[string]any{"model": "m"})); ok {
		t.Fatal("expected no usage without a response")
	}
}

func TestScalarString(t *testing.T) {
	cases := []struct {
		in   any
		want string
		ok   bool
	}{
		{"abc", "abc", true},
		{3.0, "3", true},
		{3.5, "3.5", true},
		{true, "true", true},
		{map[string]any{"a": 1}, "", false},
		{[]any{1}, "", false},
	}
	for _, c := range cases {
		got, ok := scalarString(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("scalarString(%v) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestExtractMetadata(t *testing.T) {
	pkt := packet.NewReq(map[string]any{
		"model":    "m",
		"metadata": map[string]any{"team": "core", "n": 3.0, "nested": map[string]any{"x": 1}},
	})
	pkt.Set(packet.KeyResp, map[string]any{"model": "m", "usage": map[string]any{"prompt_tokens": 1.0}})
	u, ok := Extract(pkt)
	if !ok {
		t.Fatal("expected usage")
	}
	if u.Metadata["team"] != "core" || u.Metadata["n"] != "3" {
		t.Fatalf("metadata flatten wrong: %v", u.Metadata)
	}
	if _, bad := u.Metadata["nested"]; bad {
		t.Fatalf("nested metadata should be dropped: %v", u.Metadata)
	}
}
