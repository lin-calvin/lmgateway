package config

import "strings"

// namespace 是"可寻址公共名"的授权命名空间：只有这里的名字客户端才可以直接请求，
// 每个名字带自己的 ACL。路由图（alias 目标、pool 后端、provider 绑定）是内部图，
// 不参与授权。授权在整个请求里只判定一次（ingress authorize 阶段）。
type namespace struct {
	exact         map[string]publicEntry // 精确名 → 条目（model / pool / alias）
	prefixes      map[string]publicEntry // provider 前缀公共名（仅 public）
	knownPrefixes map[string]bool        // 所有 provider 前缀（含 internal），用于 403/404
	defaultName   string
	known         map[string]bool // 所有路由可寻址名（含 internal），用于区分 403/404
}

type publicEntry struct {
	Name     string
	Kind     string // model | pool | alias | prefix
	Provider string // 仅 model/prefix 有
	Allowed  []string
	Denied   []string
}

func buildNamespace(cfg Config) *namespace {
	ns := &namespace{
		exact:         map[string]publicEntry{},
		prefixes:      map[string]publicEntry{},
		knownPrefixes: map[string]bool{},
		known:         map[string]bool{},
	}
	for _, m := range cfg.Models {
		if m.Name == "" {
			continue
		}
		ns.known[m.Name] = true
		if isPublic(m.Public) {
			ns.exact[m.Name] = publicEntry{Name: m.Name, Kind: "model", Provider: m.Provider, Allowed: m.AllowedGroups, Denied: m.DeniedGroups}
		}
		if m.Default {
			ns.defaultName = m.Name
		}
	}
	for _, p := range cfg.Pools {
		if p.Model == "" {
			continue
		}
		ns.known[p.Model] = true
		ns.exact[p.Model] = publicEntry{Name: p.Model, Kind: "pool", Allowed: p.AllowedGroups, Denied: p.DeniedGroups}
		for _, backend := range p.Backend {
			ns.known[backend] = true
		}
	}
	for _, r := range cfg.Rules {
		if name, ok := aliasName(r); ok {
			ns.known[name] = true
			ns.exact[name] = publicEntry{Name: name, Kind: "alias", Allowed: r.AllowedGroups, Denied: r.DeniedGroups}
		}
	}
	for _, p := range cfg.Providers {
		if p.Name == "" {
			continue
		}
		ns.knownPrefixes[p.Name] = true
		if isPublic(p.Public) {
			ns.prefixes[p.Name] = publicEntry{Name: p.Name, Kind: "prefix", Allowed: p.AllowedGroups, Denied: p.DeniedGroups}
		}
	}
	return ns
}

func isPublic(flag *bool) bool { return flag == nil || *flag }

// aliasName reports the public name of a pure-transform alias rule
// (from: http, no action, single `model == X` match).
func aliasName(r RuleCfg) (string, bool) {
	if r.From != "http" || r.Action != "" || len(r.Match) != 1 {
		return "", false
	}
	c := r.Match[0]
	if c.Field != "model" || c.Op != "eq" {
		return "", false
	}
	name, ok := c.Value.(string)
	return name, ok && name != ""
}

func (ns *namespace) resolve(model string) (publicEntry, bool) {
	if ns == nil {
		return publicEntry{}, false
	}
	if model == "" {
		model = ns.defaultName
	}
	if e, ok := ns.exact[model]; ok {
		return e, true
	}
	for name, e := range ns.prefixes {
		if strings.HasPrefix(model, name+"/") {
			return e, true
		}
	}
	return publicEntry{}, false
}

// knownName reports whether the name is routable at all (public or internal).
func (ns *namespace) knownName(model string) bool {
	if ns == nil {
		return false
	}
	if model == "" {
		model = ns.defaultName
	}
	if ns.known[model] {
		return true
	}
	for name := range ns.knownPrefixes {
		if strings.HasPrefix(model, name+"/") {
			return true
		}
	}
	return false
}

func (ns *namespace) empty() bool {
	return ns == nil || (len(ns.exact) == 0 && len(ns.prefixes) == 0)
}
