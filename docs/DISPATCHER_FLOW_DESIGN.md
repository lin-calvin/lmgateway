# Dispatcher Flow Refactor

## Status

Proposed implementation design. This document describes the first migration
step toward handler-owned control flow and an observability handler. It keeps
the existing packet and rule model as intact as possible.

## Goals

- Let a handler decide when and where the packet continues.
- Keep routing state (`from`, next source, and recursion depth) in the
  Dispatcher call stack, not in `packet.Packet`.
- Make streaming a normal handler route rather than a Dispatcher pause.
- Have the stream handler resume the provider result route after the event
  channel is fully consumed.
- Keep existing custom rules usable for both non-streaming and streaming
  requests.
- Keep HTTP responsible for request decoding and output adapter construction,
  not for stream consumption or Dispatcher re-entry.
- Make `observe` and `spend` ordinary handlers.

## Non-goals

- Do not introduce a public graph DSL.
- Do not put a `Dispatcher`, function closure, or continuation in a packet.
- Do not redesign `LMDocument` or `LMResponse`.
- Do not change the external provider protocol behavior in this migration.

## Handler Contract

The existing handler function receives a packet and returns a packet. It gains
one callback with an optional explicit source:

```go
type Serve func(packet.Packet, ...string) packet.Packet
type HandlerFunc func(packet.Packet, Serve) packet.Packet
```

Calling `Serve(packet)` uses the next source compiled from the currently
matched rule. Supplying one source explicitly, as in `Serve(packet, "stream")`,
enters another compiled hook and is used for branches such as a provider
choosing the stream handler.

The Dispatcher performs matching, rule `Set`, and depth accounting, then calls
the handler exactly once:

```go
nextSource := rule.To
if nextSource == "" {
    nextSource = rule.Action
}

serve := func(out packet.Packet, sources ...string) packet.Packet {
	source := nextSource
	if len(sources) > 0 && sources[0] != "" {
		source = sources[0]
	}
	return d.serve(out, source, depth+1)
}
return handler.Fn(pkt, serve)
```

The Dispatcher must not call `Serve` after the handler returns. A handler that
does not call `Serve` intentionally terminates the in-process flow.
Pure transform rules without an action remain a Dispatcher-owned special
case: after applying `Set`, the Dispatcher calls their `To` source directly.

## Packet Contract

The packet remains the business envelope. It may contain:

- request and response documents;
- `phase` (`req`, `stream`, or `resp`);
- provider identity;
- request context, start time, HTTP metadata, and reply adapter;
- error information.

It must not contain new routing control state:

- current `source` (the legacy key remains readable for compatibility, but
  the Dispatcher no longer writes or reads it);
- next source;
- stream resume source;
- recursion depth;
- a Dispatcher or callback closure.

`phase=stream` continues to identify a response event stream and lets
`ActiveDocument` select the response document. It is no longer a Dispatcher
pause condition.

## Flow

The compiled system routes are conceptually:

```text
ingress -> observe -> reply -> http
http(model=x) -> provider
provider result -> custom provider rules -> response -> spend -> respond
stream -> stream handler
```

The actual source names may use private names for ingress, reply, and stream
to prevent user rules from accidentally bypassing transport lifecycle.

### Non-streaming

```text
HTTP creates packet
  -> Serve(packet, ingress)
  -> observe starts span
  -> reply enters http
  -> http enters provider
  -> provider builds final response and calls Continue(packet)
  -> provider hook matches custom rules
  -> response
  -> spend
  -> respond
  -> reply writes JSON
  -> observe ends span
```

### Streaming

```text
HTTP creates packet with a reply adapter
  -> Serve(packet, ingress)
  -> observe starts span
  -> reply enters http
  -> http enters provider
  -> provider builds LMResponse and calls Serve(packet, stream)
  -> stream consumes resp.Get("event") and writes events through Reply
  -> stream defer changes phase to resp
  -> stream defer calls Serve(packet, provider result hook)
  -> custom provider rules
  -> response
  -> spend
  -> respond
  -> stream returns through reply and observe
```

The stream handler returns the same packet after the stream is drained. There
is no response-only packet and no second Dispatcher invocation in HTTP.

The stream handler must defer the result route so all exits are covered:
normal terminal event, EOF, parser error, client cancellation, and write
failure. The reply adapter records whether output was committed. Once an SSE
response has started, a later error cannot change the HTTP status or become a
second JSON response.

## Custom Rules

Custom rules continue to match packet data at public sources. For example:

```yaml
- id: redact-openai
  from: openai
  match:
    - field: model
      op: eq
      value: agent
  action: redact
  to: response
```

Both normal and streaming provider paths re-enter the `openai` source after
provider completion, so both match `redact-openai`.

The compiler must preserve the existing `from`, `match`, `action`, `to`, and
`set` rule input. A future RouteIntent compiler may add an `after` dependency,
but this migration must not require a new public configuration format.

## HTTP Boundary

HTTP constructs the request document, packet, context, start time, metadata,
and reply adapter, then starts the Dispatcher at the private ingress source.

HTTP must not:

- inspect `PhaseStream` to decide control flow;
- consume `LMResponse` events;
- construct a response-only packet;
- call Dispatcher again after stream completion.

The reply adapter remains HTTP-specific. A future non-HTTP transport can
provide another adapter without changing provider or stream handlers.

## Observe Handler

`observe` is a normal wrapper handler at ingress. It starts an OpenTelemetry
LLM span, calls its `Continue`, and ends the span after the entire downstream
flow returns. Since the stream handler blocks until its deferred result route
has completed, the span covers stream output, final response aggregation,
spend, and terminal handling.

The first implementation uses standard OpenTelemetry OTLP export and generic
`gen_ai.*` attributes so both Langfuse and Phoenix can receive the data. It
does not use a backend-specific SDK.

Default attributes are limited to provider type, request/upstream model,
response ID/model, known token usage, finish reasons, and controlled error
types. Prompt, completion, reasoning content, tool arguments, credentials,
raw upstream errors, and arbitrary HTTP metadata are not captured by default.

## Spend Handler

`spend` remains a separate response-stage handler. It reads normalized usage
from the final response and records the existing business spend ledger. It
must call `Continue` after recording. Observability and spend are independent:
OTel spans are not the source of truth for billing.

## Migration Order

1. Add handler callback types and migrate Dispatcher/Registry/Table matching.
2. Migrate simple handlers and provider handlers to call `Continue` or
   `Serve` explicitly.
3. Move stream event consumption into a transport-independent stream handler
   using the packet reply adapter; remove HTTP response-only re-entry.
4. Add regression coverage for custom rules, normal/streaming parity,
   cancellation, and exactly-once spend.
5. Add `observe` as an ordinary ingress wrapper and implement its OTel span
   lifecycle.
6. Only after behavior is stable, extract automatic rule generation into an
   internal compiler if needed.

## Compatibility and Safety

- Existing `RuleCfg` remains the external input.
- Existing action names remain valid; `usage` remains an alias for spend.
- User rules must remain able to transform and reroute packets.
- Dispatcher depth protection remains local to the call stack.
- A handler must not call a continuation more than once for the same packet.
- Stream handling must remain synchronous with the request flow to avoid
  packet map races and continuation lifetime ambiguity.
- Runtime reload must capture the Dispatcher used by the request; an active
  stream must not switch to a newly compiled runtime midway through output.
