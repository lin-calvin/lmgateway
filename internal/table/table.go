package table

import (
	"lmgateway/internal/match"
	"lmgateway/internal/packet"
)

// Rule 一条规则：从位置 From 出发，内容命中则应用 Set（改写包字段）、运行 Action handler，
// 下一位置为 To（默认 = Action）。
// 无 priority/phase —— 位置（From）隐含相位；用户边在自动边之前（分层），层内声明序。
// Set+To（无 Action）= 纯变换规则：改写请求后回走，让后续正常路由接手。
type Rule struct {
	ID        string
	From      string // 起始位置（包上 source），空 = 任意位置兜底（终态用）
	Match     match.Matcher
	Set       map[string]any // 硬覆盖：action 前无条件改写包字段（点分路径）
	Default   map[string]any // 软默认：仅当字段缺失/为空时写入，客户端显式值优先
	Action    string         // handler 名 或 终态（respond/error）；纯变换规则可空
	To        string         // 下一位置名；默认 = Action，无 Action 时必填
	Generated bool           // auto（model 实体生成） vs user
}

// Table 编译后的最终规则表：按 from 索引，每 from 内保序（用户层在前，自动层在后）。
// dispatcher 无状态：只按包的 source（位置）查边，从不解析图。
type Table struct {
	rules  []Rule
	byFrom map[string][]int
}

func New() *Table {
	return &Table{rules: []Rule{}, byFrom: map[string][]int{}}
}

func (t *Table) Add(r Rule) {
	t.rules = append(t.rules, r)
	t.byFrom[r.From] = append(t.byFrom[r.From], len(t.rules)-1)
}

// Match 返回当前位置下第一条内容命中的规则；无则回退任意位置兜底（From=""）。nil = table-miss。
func (t *Table) Match(source string, pkt packet.Packet) *Rule {
	if r := firstMatch(t, t.byFrom[source], pkt); r != nil {
		return r
	}
	return firstMatch(t, t.byFrom[""], pkt)
}

func firstMatch(t *Table, idx []int, pkt packet.Packet) *Rule {
	for _, i := range idx {
		r := &t.rules[i]
		if len(r.Match.Conds) > 0 && !r.Match.Match(pkt) {
			continue
		}
		return r
	}
	return nil
}

// All 返回全部规则（list/meta 用）
func (t *Table) All() []Rule {
	out := make([]Rule, len(t.rules))
	copy(out, t.rules)
	return out
}
