package tenancy

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// randomSuffix 生成 8 位十六进制随机后缀。
func randomSuffix() string {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(raw)
}

// userIDFromEmail 从邮箱派生一个合法 id(slug 规则:小写字母数字与连字符)。
// 邮箱本身唯一,但 slug 会丢失信息,因此追加随机后缀避免碰撞。
func userIDFromEmail(email string) string {
	local := email
	if at := strings.Index(email, "@"); at > 0 {
		local = email[:at]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 24 {
		slug = slug[:24]
	}
	if len(slug) < 2 {
		slug = "u"
	}
	return "u-" + slug + "-" + randomSuffix()
}
