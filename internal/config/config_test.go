package config

import (
	"context"
	"testing"

	"lmgateway/internal/store/mem"
)

// TestServerSettingCannotOverrideMasterKey 是一条**安全不变量**：
// master_key 的 tag 是 json:"-"，所以设置文档（DB 覆盖）既不能回显它，
// 也不能改写它——即使有人往文档里塞 master_key，加载出来的仍是 YAML 基线的值。
//
// 这条测试会在有人把 tag 改成可写时失败：主密钥是启动凭据，
// 让它能经管理 API 改写等于给"改配置"开了提权口子。
func TestServerSettingCannotOverrideMasterKey(t *testing.T) {
	ctx := context.Background()
	js := mem.NewJSON()
	base := Config{Server: ServerCfg{Addr: ":8080", MasterKey: "sk-baseline-master"}}

	// 恶意/误操作：往设置文档里塞一个 master_key
	if _, err := js.Put(ctx, ServerSettingKey, map[string]any{
		"addr": ":9090", "master_key": "sk-injected", "source": SourceDBOverride,
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := ConfigFromStoreWithBase(ctx, js, base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Fatalf("addr override should apply, got %q", cfg.Server.Addr)
	}
	if cfg.Server.MasterKey != "sk-baseline-master" {
		t.Fatalf("设置文档不能改写主密钥，got %q", cfg.Server.MasterKey)
	}
}
