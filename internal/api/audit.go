package api

import (
	"context"
	"encoding/json"

	"lmgateway/internal/audit"
	"lmgateway/internal/config"
)

// 本文件集中放置"配置写操作的审计埋点"。
//
// 为什么埋在 handler 层而不是存储层:存储层看不到身份与语义——JSONStore 的
// Put 分不清"创建 provider"和"改模型价格",也拿不到 api_key 是否被替换这类
// 业务含义。handler 层有 ctx(身份)、有 before/after、有资源名,能写出
// "谁把 model/gpt-x 的 input_per_mtok 从 3 改成 5"这种可读记录。
//
// 中间件是兜底(任何写方法都会留一条);这里是语义增强,同一次请求只落一条。

// recordConfigWrite 记录一次配置写入。
//
//	action 形如 "model.patch"、"setting.set"
//	key    形如 "model/gpt-x"、"setting/display"
//	before 写前已解析的文档(map 或结构体均可,nil 表示新增)
//	after  本次要落库的文档
func (a *api) recordConfigWrite(ctx context.Context, action, key string, before, after any, extra map[string]any) {
	detail := map[string]any{"path": key}
	for k, v := range extra {
		detail[k] = v
	}
	beforeMap := configMap(before)
	afterMap := configMap(after)
	if beforeMap != nil || afterMap != nil {
		if changes := audit.Diff(beforeMap, afterMap); changes != nil {
			detail["changes"] = changes
		}
	}
	audit.Record(ctx, action, key, detail)
}

// resolvedConfigMap 读取某个配置键的当前已解析文档(含 YAML 基线),用于 diff。
func (a *api) resolvedConfigMap(ctx context.Context, key string) any {
	item, err := config.ResolveItem(ctx, a.deps.Manager.Storage().JSON, a.deps.Manager.Base(), key)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(item.Data, &out); err != nil {
		return nil
	}
	return out
}

// configMap 把结构体/指针转成通用 map,便于逐字段 diff。
func configMap(v any) any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
