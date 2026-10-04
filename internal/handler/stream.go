package handler

import (
	"lmgateway/internal/dispatch"
	"lmgateway/internal/packet"
)

// Stream consumes the LMResponse event channel through the transport adapter.
// Once the channel is drained, the same packet returns to its provider hook so
// normal response rules and spend handling apply to both stream modes.
type Stream struct{}

func (Stream) Handle(pkt packet.Packet, serve dispatch.Serve) (out packet.Packet) {
	defer func() {
		out.SetPhase(packet.PhaseResp)
		provider, _ := out.Str(packet.KeyProvider)
		out = serve(out, provider)
	}()

	reply, ok := pkt[packet.KeyReply].(packet.Reply)
	if !ok {
		return pkt.Fail(packet.ErrInternal, "missing stream reply adapter")
	}
	return reply.WriteStream(pkt)
}
