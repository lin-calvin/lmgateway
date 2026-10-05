package api

import (
	"encoding/json"
	"fmt"
	"reflect"

	"lmgateway/internal/config"
)

// MaskedSecret 读取敏感字段时用的占位值。
//
// 写入路径会**显式拒绝**这个字面量（见 applyAndWrite）：客户端把 GET 拿到的打码值
// 原样写回，会静默把真实密钥覆盖成 "****"，而且 schema 拦不住。宁可报错让人看见。
const MaskedSecret = "****"

// Entity 一个 handler 实体（provider/model...）：settings 文档 + 语义校验钩子
type Entity struct {
	Kind    string
	Prefix  string
	DocType any // 零值，用于 JSON Schema 生成 + 新实例
	// Required 必填字段（非空检查；name 取自路径，不在此列）
	Required []string
	// Apply 把文档应用到候选 config（同名 upsert）
	Apply func(cfg *config.Config, name string, doc any) error
	// Remove 从候选 config 移除
	Remove func(cfg *config.Config, name string) error
	// Mask GET/list 时打码敏感字段（如 api_key）
	Mask func(m map[string]any) map[string]any
}

// New 创建 DocType 的新实例（指针）
func (e *Entity) New() any {
	return reflect.New(reflect.TypeOf(e.DocType)).Interface()
}

// checkRequired 校验必填字段非空
func (e *Entity) checkRequired(doc any) error {
	if len(e.Required) == 0 {
		return nil
	}
	b, _ := json.Marshal(doc)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, f := range e.Required {
		v, ok := m[f]
		if !ok || fmt.Sprint(v) == "" {
			return fmt.Errorf("field %q is required", f)
		}
	}
	return nil
}

func providerEntity() *Entity {
	return &Entity{
		Kind:     "provider",
		Prefix:   "provider/",
		DocType:  config.ProviderCfg{},
		Required: []string{"type", "base_url"},
		Apply: func(cfg *config.Config, name string, doc any) error {
			p := doc.(*config.ProviderCfg)
			p.Name = name
			for i := range cfg.Providers {
				if cfg.Providers[i].Name == name {
					cfg.Providers[i] = *p
					return nil
				}
			}
			cfg.Providers = append(cfg.Providers, *p)
			return nil
		},
		Remove: func(cfg *config.Config, name string) error {
			var out []config.ProviderCfg
			for _, p := range cfg.Providers {
				if p.Name != name {
					out = append(out, p)
				}
			}
			if len(out) == len(cfg.Providers) {
				return fmt.Errorf("provider %q not found", name)
			}
			cfg.Providers = out
			return nil
		},
		Mask: func(m map[string]any) map[string]any {
			if k, ok := m["api_key"].(string); ok && k != "" {
				m["api_key"] = MaskedSecret
				m["has_api_key"] = true
			}
			return m
		},
	}
}

func poolEntity() *Entity {
	return &Entity{
		Kind:     "pool",
		Prefix:   "pool/",
		DocType:  config.PoolCfg{},
		Required: []string{"model", "backend"},
		Apply: func(cfg *config.Config, name string, doc any) error {
			p := doc.(*config.PoolCfg)
			p.Model = name
			for i := range cfg.Pools {
				if cfg.Pools[i].Model == name {
					cfg.Pools[i] = *p
					return nil
				}
			}
			cfg.Pools = append(cfg.Pools, *p)
			return nil
		},
		Remove: func(cfg *config.Config, name string) error {
			var out []config.PoolCfg
			for _, p := range cfg.Pools {
				if p.Model != name {
					out = append(out, p)
				}
			}
			if len(out) == len(cfg.Pools) {
				return fmt.Errorf("pool %q not found", name)
			}
			cfg.Pools = out
			return nil
		},
	}
}

func modelEntity() *Entity {
	return &Entity{
		Kind:     "model",
		Prefix:   "model/",
		DocType:  config.ModelCfg{},
		Required: []string{"provider"},
		Apply: func(cfg *config.Config, name string, doc any) error {
			m := doc.(*config.ModelCfg)
			m.Name = name
			for i := range cfg.Models {
				if cfg.Models[i].Name == name {
					cfg.Models[i] = *m
					return nil
				}
			}
			cfg.Models = append(cfg.Models, *m)
			return nil
		},
		Remove: func(cfg *config.Config, name string) error {
			var out []config.ModelCfg
			for _, m := range cfg.Models {
				if m.Name != name {
					out = append(out, m)
				}
			}
			if len(out) == len(cfg.Models) {
				return fmt.Errorf("model %q not found", name)
			}
			cfg.Models = out
			return nil
		},
	}
}
