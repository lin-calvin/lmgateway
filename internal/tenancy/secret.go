package tenancy

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// 密码哈希参数。pbkdf2 是 Go 1.24+ 标准库(crypto/pbkdf2),因此不引入新依赖。
const (
	pbkdf2Iterations = 210_000
	pbkdf2KeyLen     = 32
	pbkdf2SaltLen    = 16
	pbkdf2Scheme     = "pbkdf2-sha256"
)

// HashPassword 生成 `pbkdf2-sha256$<iter>$<salt-b64>$<hash-b64>` 形式的密码哈希。
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", fmt.Errorf("password must be at least 8 characters")
	}
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Scheme, pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// VerifyPassword 常量时间校验密码。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Scheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NewAPIKey 生成一个新的 API key,返回明文与可直接入库的凭据信息。
// 明文只在创建/轮换的响应里出现一次。
func NewAPIKey() (plaintext, hash, prefix string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", "", err
	}
	plaintext = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash = HashSecret(plaintext)
	prefix = DisplayPrefix(plaintext)
	return plaintext, hash, prefix, nil
}

// NewSessionToken 生成会话 token 的明文与哈希。
func NewSessionToken() (plaintext, hash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	plaintext = SessionTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return plaintext, HashSecret(plaintext), nil
}

// HashSecret 对高熵密钥做摘要。API key / session token 都是 256bit 随机串,
// 不存在字典攻击面,因此用单次 sha256 即可(不需要 pbkdf2,那是给低熵密码用的)。
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// DisplayPrefix 返回给前端展示的短前缀,永不回显完整密钥。
func DisplayPrefix(plaintext string) string {
	const keep = 14
	if len(plaintext) <= keep {
		return plaintext
	}
	return plaintext[:keep] + "…"
}

// LooksLikeAPIKey / LooksLikeSession 用于在查库前按前缀分流凭据类型。
func LooksLikeAPIKey(token string) bool  { return strings.HasPrefix(token, APIKeyPrefix) }
func LooksLikeSession(token string) bool { return strings.HasPrefix(token, SessionTokenPrefix) }
