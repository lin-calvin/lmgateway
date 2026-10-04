package dispatch_test

import (
	"testing"

	"lmgateway/internal/dispatch"
	"lmgateway/internal/match"
	"lmgateway/internal/packet"
	"lmgateway/internal/table"
)

// build a mini gateway: http -> echo(flip resp, mutate content) -> global respond fallback
func buildDispatcher(t *testing.T, mutate func(*table.Table, *dispatch.Registry)) *dispatch.Dispatcher {
	t.Helper()
	tbl := table.New()
	reg := dispatch.NewRegistry()

	reg.Register("echo", func(p packet.Packet) packet.Packet {
		req, _ := p.Map("req")
		p.Set("resp", map[string]any{"content": "echo:" + req["msg"].(string)})
		p.SetPhase(packet.PhaseResp)
		return p
	})

	tbl.Add(table.Rule{From: "http", Action: "echo"})
	tbl.Add(table.Rule{Action: dispatch.ActionRespond}) // 全局兜底

	if mutate != nil {
		mutate(tbl, reg)
	}
	return dispatch.New(tbl, reg)
}

func TestFullChain(t *testing.T) {
	d := buildDispatcher(t, nil)

	pkt := packet.NewReq(map[string]any{"msg": "hi"})
	out := d.Serve(pkt, packet.SourceHTTP)

	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	resp, _ := out.Map("resp")
	if resp["content"] != "echo:hi" {
		t.Errorf("resp content mismatch: %v", resp)
	}
}

func TestHandlerOwnsContinuation(t *testing.T) {
	tbl := table.New()
	reg := dispatch.NewRegistry()
	called := false
	reg.Register("first", dispatch.HandlerFunc(func(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
		return serve(pkt)
	}))
	reg.Register("second", dispatch.HandlerFunc(func(pkt packet.Packet, serve dispatch.Serve) packet.Packet {
		called = true
		return serve(pkt)
	}))
	tbl.Add(table.Rule{From: "http", Action: "first", To: "second"})
	tbl.Add(table.Rule{From: "second", Action: "second", To: "done"})
	tbl.Add(table.Rule{From: "done", Action: dispatch.ActionRespond})
	out := dispatch.New(tbl, reg).Serve(packet.NewReq(map[string]any{}), packet.SourceHTTP)
	if !called {
		t.Fatalf("handler continuation was not followed: called=%v phase=%s", called, out.Phase())
	}
}

// 位置分区：http 边只在位置 http 生效；echo 后位置变为 echo，req 边不再命中（结构分区，无 phase 字段）
func TestPositionPartition(t *testing.T) {
	d := buildDispatcher(t, func(tbl *table.Table, reg *dispatch.Registry) {
		// 额外一条 from=http 的规则，匹配 req.model 存在 → echo
		tbl.Add(table.Rule{
			From:   "http",
			Match:  match.All(match.Condition{Field: "req.model", Op: match.OpExists}),
			Action: "echo",
		})
	})

	pkt := packet.NewReq(map[string]any{"msg": "hi", "model": "x"})
	out := d.Serve(pkt, packet.SourceHTTP)

	// 第一次 echo 后位置="echo"，不会再走 http 边 → content 不会 echo:echo:hi
	resp, _ := out.Map("resp")
	if resp["content"] != "echo:hi" {
		t.Errorf("position partition failed, resp was re-processed: %v", resp)
	}
}

// to 字段：显式命名下一位置（跑 echo handler，但位置命名为 stage-1）
func TestToField(t *testing.T) {
	tbl := table.New()
	reg := dispatch.NewRegistry()
	reg.Register("echo", func(p packet.Packet) packet.Packet {
		p.Set("resp", map[string]any{"content": "ok"})
		p.SetPhase(packet.PhaseResp)
		return p
	})
	reg.Register("audit", func(p packet.Packet) packet.Packet {
		req, _ := p.Map("req")
		req["audited"] = true
		return p
	})
	tbl.Add(table.Rule{From: "http", Action: "audit", To: "audit"}) // 跑 audit handler，位置=audit
	tbl.Add(table.Rule{From: "audit", Action: "echo", To: "done"})  // 位置=audit → echo，位置=done
	tbl.Add(table.Rule{Action: dispatch.ActionRespond})
	d := dispatch.New(tbl, reg)

	out := d.Serve(packet.NewReq(map[string]any{"msg": "hi"}), packet.SourceHTTP)
	if _, ok := out.Error(); ok {
		t.Fatal("unexpected error")
	}
	if req, _ := out.Map("req"); req["audited"] != true {
		t.Error("audit handler should have run")
	}
}

func TestRuleDefaultDoesNotOverrideClient(t *testing.T) {
	tbl := table.New()
	reg := dispatch.NewRegistry()
	reg.Register("echo", func(p packet.Packet) packet.Packet {
		p.SetPhase(packet.PhaseResp)
		return p
	})
	tbl.Add(table.Rule{
		From:    "http",
		Action:  "echo",
		Set:     map[string]any{"model": "server-model"},
		Default: map[string]any{"reasoning.effort": "max"},
	})
	tbl.Add(table.Rule{Action: dispatch.ActionRespond})

	out := dispatch.New(tbl, reg).Serve(packet.NewReq(map[string]any{
		"model":            "client-model",
		"reasoning_effort": "low",
		"messages":         []any{},
	}), packet.SourceHTTP)
	req, ok := out.Request()
	if !ok {
		t.Fatal("missing request document")
	}
	if value, _ := req.Get("reasoning.effort"); value != "low" {
		t.Fatalf("client reasoning should win over default: %v", value)
	}
	if value, _ := req.Get("model"); value != "server-model" {
		t.Fatalf("hard set should override client model: %v", value)
	}
}

func TestRuleDefaultFillsAbsent(t *testing.T) {
	tbl := table.New()
	reg := dispatch.NewRegistry()
	reg.Register("echo", func(p packet.Packet) packet.Packet {
		p.SetPhase(packet.PhaseResp)
		return p
	})
	tbl.Add(table.Rule{From: "http", Action: "echo", Default: map[string]any{"reasoning.effort": "max"}})
	tbl.Add(table.Rule{Action: dispatch.ActionRespond})

	out := dispatch.New(tbl, reg).Serve(packet.NewReq(map[string]any{
		"model":    "m",
		"messages": []any{},
	}), packet.SourceHTTP)
	req, _ := out.Request()
	if value, _ := req.Get("reasoning.effort"); value != "max" {
		t.Fatalf("default should fill absent reasoning: %v", value)
	}
}

// error packet (resp phase) at provider position → 走到全局 respond 返回
func TestErrorPacket(t *testing.T) {
	d := buildDispatcher(t, nil)

	pkt := packet.NewReq(map[string]any{"msg": "hi"})
	pkt = pkt.WithError("upstream boom")
	out := d.Serve(pkt, "echo") // 错误包在 provider 位置重入（真实流：provider 失败回投）

	if msg, ok := out.Error(); !ok || msg != "upstream boom" {
		t.Errorf("error message should pass through: %q", msg)
	}
}

// table-miss：位置 http 无规则 → no_route（404 语义）
func TestNoRouteOnMiss(t *testing.T) {
	tbl := table.New()
	reg := dispatch.NewRegistry()
	reg.Register("echo", func(p packet.Packet) packet.Packet { return p })
	tbl.Add(table.Rule{From: "http", Match: match.All(match.Condition{Field: "req.model", Op: match.OpEq, Value: "gpt"}), Action: "echo"})
	d := dispatch.New(tbl, reg)

	out := d.Serve(packet.NewReq(map[string]any{"model": "other"}), packet.SourceHTTP)
	if kind, _ := out.ErrorKind(); kind != packet.ErrNoRoute {
		t.Fatalf("expected no_route, got %q", kind)
	}
}

// loop guard: 回投自身位置 → MaxDepth truncates
func TestMaxDepthGuard(t *testing.T) {
	tbl := table.New()
	reg := dispatch.NewRegistry()
	reg.Register("a", func(p packet.Packet) packet.Packet { return p }) // 不翻 phase，原样回投
	tbl.Add(table.Rule{From: "http", Action: "a"})                      // 位置 http → a；a 处理完位置=a
	tbl.Add(table.Rule{From: "a", Action: "a"})                         // 位置 a → a（自环）
	d := dispatch.New(tbl, reg)
	d.MaxDepth = 20

	out := d.Serve(packet.NewReq(map[string]any{}), packet.SourceHTTP)
	if _, ok := out.Error(); !ok {
		t.Fatal("loop should be truncated into an error packet by MaxDepth")
	}
}

// set 原语 + to 回走：alias 规则改写 req.model 后回 http，正常路由接手
func TestSetTransformLoopback(t *testing.T) {
	tbl := table.New()
	reg := dispatch.NewRegistry()
	reg.Register("echo", func(p packet.Packet) packet.Packet {
		req, _ := p.Map("req")
		p.Set("resp", map[string]any{"content": req["model"]})
		p.SetPhase(packet.PhaseResp)
		return p
	})
	// alias：agent → 改写 req.model=gpt + 回走 http
	tbl.Add(table.Rule{
		From:  "http",
		Match: match.All(match.Condition{Field: "req.model", Op: match.OpEq, Value: "agent"}),
		Set:   map[string]any{"model": "gpt"},
		To:    "http",
	})
	// 正常路由：gpt → echo
	tbl.Add(table.Rule{
		From:   "http",
		Match:  match.All(match.Condition{Field: "req.model", Op: match.OpEq, Value: "gpt"}),
		Action: "echo",
	})
	tbl.Add(table.Rule{Action: dispatch.ActionRespond})
	d := dispatch.New(tbl, reg)

	out := d.Serve(packet.NewReq(map[string]any{"model": "agent"}), packet.SourceHTTP)
	if msg, ok := out.Error(); ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	resp, _ := out.Map("resp")
	if resp["content"] != "gpt" {
		t.Errorf("alias should rewrite req.model to gpt, got %v", resp["content"])
	}
}
