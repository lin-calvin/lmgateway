package match

import (
	"fmt"
	"regexp"
	"strings"

	"lmgateway/internal/packet"
)

type Op string

const (
	OpEq     Op = "eq"
	OpNeq    Op = "neq"
	OpIn     Op = "in"
	OpPrefix Op = "prefix"
	OpExists Op = "exists"
	OpRegex  Op = "regex"
	OpEmpty  Op = "empty"
)

type Condition struct {
	Field string
	Op    Op
	Value any
	re    *regexp.Regexp
}

type Matcher struct{ Conds []Condition }

func All(conds ...Condition) Matcher { return Matcher{Conds: conds} }

func Compile(field string, op Op, value any) (Condition, error) {
	c := Condition{Field: field, Op: op, Value: value}
	switch op {
	case OpEq, OpNeq, OpIn, OpPrefix, OpExists, OpEmpty:
	case OpRegex:
		s, ok := value.(string)
		if !ok {
			return c, fmt.Errorf("regex op requires string value: field=%s", field)
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return c, fmt.Errorf("regex compile failed field=%s: %w", field, err)
		}
		c.re = re
	default:
		return c, fmt.Errorf("unknown match operator %q (field=%s)", op, field)
	}
	return c, nil
}

func (m Matcher) Match(pkt any) bool {
	for _, c := range m.Conds {
		if !c.match(pkt) {
			return false
		}
	}
	return true
}

func (c Condition) match(pkt any) bool {
	v, ok := resolve(pkt, c.Field)
	switch c.Op {
	case OpEmpty:
		return !ok || toString(v) == ""
	case OpExists:
		return ok
	case OpEq:
		return ok && equal(v, c.Value)
	case OpNeq:
		return !ok || !equal(v, c.Value)
	case OpIn:
		if !ok {
			return false
		}
		for _, item := range toSlice(c.Value) {
			if equal(v, item) {
				return true
			}
		}
		return false
	case OpPrefix:
		return ok && strings.HasPrefix(toString(v), toString(c.Value))
	case OpRegex:
		return ok && c.re != nil && c.re.MatchString(toString(v))
	}
	return false
}

func resolve(value any, field string) (any, bool) {
	if pkt, ok := value.(packet.Packet); ok {
		return pkt.Resolve(field)
	}
	if pkt, ok := value.(map[string]any); ok {
		if doc, ok := pkt[packet.KeyReq].(interface{ Get(string) (any, bool) }); ok {
			if field == "type" {
				return nil, false
			}
			if strings.HasPrefix(field, "req.") || strings.HasPrefix(field, "resp.") {
				field = field[4:]
			}
			return doc.Get(field)
		}
		return resolveMap(pkt, field)
	}
	return nil, false
}

func resolveMap(pkt map[string]any, field string) (any, bool) {
	parts := strings.Split(field, ".")
	var current any = pkt
	for _, part := range parts {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func equal(a, b any) bool {
	switch av := a.(type) {
	case float64:
		if bv, ok := b.(float64); ok {
			return av == bv
		}
	case string:
		if bv, ok := b.(string); ok {
			return av == bv
		}
	case bool:
		if bv, ok := b.(bool); ok {
			return av == bv
		}
	}
	return toString(a) == toString(b)
}

func toSlice(v any) []any {
	if list, ok := v.([]any); ok {
		return list
	}
	return nil
}
func toString(v any) string { return fmt.Sprint(v) }
