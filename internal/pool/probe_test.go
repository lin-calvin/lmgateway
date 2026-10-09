package pool

import (
	"context"
	"testing"

	"lmgateway/internal/lm"
	"lmgateway/internal/packet"
)

func TestDiscardReplyTimestamps(t *testing.T) {
	events := make(chan lm.LMEvent, 2)
	events <- lm.NewChatEvent(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": "hi"}}},
	})
	events <- lm.NewChatEvent(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{}}},
	})
	close(events)

	stream, err := lm.NewResponseStream("openai", events, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pkt := packet.NewReq(map[string]any{"model": "backend"})
	pkt.Set(packet.KeyResp, stream)

	reply := &discardReply{}
	out := reply.WriteStream(pkt)
	if _, ok := out[packet.KeyFirst]; !ok {
		t.Fatal("discard reply should stamp first-output time")
	}
	if _, ok := out[packet.KeyEnd]; !ok {
		t.Fatal("discard reply should stamp end time")
	}
	if !reply.Committed() {
		t.Fatal("discard reply should be committed")
	}
}

func TestProbePromptIsUnique(t *testing.T) {
	a := probePrompt()
	b := probePrompt()
	if a == b {
		t.Fatal("probe prompt must differ between runs (cache-busting timestamp)")
	}
}
