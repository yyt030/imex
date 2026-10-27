package common

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// 这些用例钉住 2026-10-26 密钥派生改动的语义：PBKDF2-HMAC-SHA256 + 每条密文
// 独立随机盐，AES-256-GCM 不变，密文布局为 base64(salt || nonce || ct+tag)，
// 并且**不保留旧布局的解密路径**（旧串一律认证失败，不报错成"看起来像明文"的东西）。

const testKey = "correct horse battery staple"

func TestEncryptDecryptRoundtrip(t *testing.T) {
	cases := []struct {
		name string
		plat string
	}{
		{"ascii", "p@ssw0rd-123"},
		{"空口令", ""},
		{"含冒号与前缀", "ENC:not-encrypted"},
		{"CJK 与 emoji", "密码🔑"},
		{"长口令", strings.Repeat("x", 256)},
		{"控制字符", "a\tb\x00c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blob, err := EncryptPassword(tc.plat, testKey)
			if err != nil {
				t.Fatalf("encrypt: %v", err)
			}
			if !IsEncryptedPassword(blob) {
				t.Fatalf("密文缺少 %s 前缀: %s", EncryptedPasswordPrefix, blob)
			}
			got, err := DecryptPassword(blob, testKey)
			if err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if got != tc.plat {
				t.Errorf("往返不一致: got %q want %q", got, tc.plat)
			}
		})
	}
}

// TestBlobLayout 检查前置结构：salt(kdfSaltSize) + GCM nonce + 明文 + 16 字节标签。
func TestBlobLayout(t *testing.T) {
	const plaintext = "layout-check"
	blob, err := EncryptPassword(plaintext, testKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(blob, EncryptedPasswordPrefix))
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	block, err := aes.NewCipher(make([]byte, kdfKeySize))
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	want := kdfSaltSize + gcm.NonceSize() + len(plaintext) + gcm.Overhead()
	if len(raw) != want {
		t.Errorf("密文长度 = %d, 期望 salt(%d)+nonce(%d)+明文(%d)+标签(%d) = %d",
			len(raw), kdfSaltSize, gcm.NonceSize(), len(plaintext), gcm.Overhead(), want)
	}
}

// TestEncryptIsRandom 同时覆盖随机盐与随机 nonce：同一密钥同一明文两次加密必须
// 整体不同，且两条密文都能解回同一明文（密文不可当等值键比较）。
func TestEncryptIsRandom(t *testing.T) {
	a, err := EncryptPassword("same-password", testKey)
	if err != nil {
		t.Fatalf("encrypt a: %v", err)
	}
	b, err := EncryptPassword("same-password", testKey)
	if err != nil {
		t.Fatalf("encrypt b: %v", err)
	}
	if a == b {
		t.Fatal("同一明文两次加密得到相同密文：盐或 nonce 未随机")
	}
	ra, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, EncryptedPasswordPrefix))
	rb, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(b, EncryptedPasswordPrefix))
	if string(ra[:kdfSaltSize]) == string(rb[:kdfSaltSize]) {
		t.Error("两条密文的盐相同：salt 未每次新生成")
	}
	for _, blob := range []string{a, b} {
		got, err := DecryptPassword(blob, testKey)
		if err != nil || got != "same-password" {
			t.Errorf("解密失败: got %q err %v", got, err)
		}
	}
}

// TestSameKeyDifferentSaltDerivesDifferentKeys 是加盐的实际目的：旧实现里同一
// 密钥对所有密文派生出同一把 AES 密钥，破一条即破全部。
func TestSameKeyDifferentSaltDerivesDifferentKeys(t *testing.T) {
	blob, err := EncryptPassword("x", testKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(blob, EncryptedPasswordPrefix))
	salt1, salt2 := raw[:kdfSaltSize], []byte{}
	other, err := EncryptPassword("x", testKey)
	if err != nil {
		t.Fatalf("encrypt other: %v", err)
	}
	rawOther, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(other, EncryptedPasswordPrefix))
	salt2 = rawOther[:kdfSaltSize]
	k1, err := deriveKey(testKey, salt1)
	if err != nil {
		t.Fatalf("derive 1: %v", err)
	}
	k2, err := deriveKey(testKey, salt2)
	if err != nil {
		t.Fatalf("derive 2: %v", err)
	}
	if string(k1) == string(k2) {
		t.Error("不同盐派生出相同密钥：盐未参与派生")
	}
	if len(k1) != kdfKeySize {
		t.Errorf("派生密钥长度 = %d, 期望 %d (AES-256)", len(k1), kdfKeySize)
	}
}

func TestDecryptWrongKey(t *testing.T) {
	blob, err := EncryptPassword("secret", testKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	_, err = DecryptPassword(blob, "wrong-key")
	if err == nil {
		t.Fatal("错误密钥竟然解密成功")
	}
	if !strings.Contains(err.Error(), "wrong key or corrupted data") {
		t.Errorf("错误文案外泄了解密失败的具体原因: %v", err)
	}
}

// TestDecryptDetectsTampering 覆盖标签保护的两种破坏方式：改一字节与截断。
func TestDecryptDetectsTampering(t *testing.T) {
	blob, err := EncryptPassword("tamper-me", testKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(blob, EncryptedPasswordPrefix))

	t.Run("翻转密文一字节", func(t *testing.T) {
		mutated := append([]byte{}, raw...)
		mutated[len(mutated)-1] ^= 0x01
		if _, err := DecryptPassword(EncryptedPasswordPrefix+base64.StdEncoding.EncodeToString(mutated), testKey); err == nil {
			t.Fatal("密文被篡改仍然解密成功")
		}
	})

	t.Run("翻转盐一字节", func(t *testing.T) {
		mutated := append([]byte{}, raw...)
		mutated[0] ^= 0x01
		if _, err := DecryptPassword(EncryptedPasswordPrefix+base64.StdEncoding.EncodeToString(mutated), testKey); err == nil {
			t.Fatal("盐被篡改仍然解密成功（应因派生密钥不同而认证失败）")
		}
	})

	t.Run("截断", func(t *testing.T) {
		short := EncryptedPasswordPrefix + base64.StdEncoding.EncodeToString(raw[:kdfSaltSize+4])
		if _, err := DecryptPassword(short, testKey); err == nil {
			t.Fatal("过短的密文竟然没有报错")
		}
	})

	t.Run("非法 base64", func(t *testing.T) {
		if _, err := DecryptPassword(EncryptedPasswordPrefix+"!!!not-base64!!!", testKey); err == nil {
			t.Fatal("非法 base64 竟然没有报错")
		}
	})
}

// legacyEncrypt 复刻改动前的布局：密钥 = sha256(IMEX_KEY)，密文 = base64(nonce||ct||tag)。
// 用户明确要求不兼容、不加版本位，因此本用例钉的是"旧串必须失败"。
func legacyEncrypt(t *testing.T, plaintext, key string) string {
	t.Helper()
	h := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(h[:])
	if err != nil {
		t.Fatalf("legacy aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("legacy gcm: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("legacy nonce: %v", err)
	}
	return EncryptedPasswordPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plaintext), nil))
}

func TestLegacyBlobIsRejected(t *testing.T) {
	const plaintext = "pre-pbkdf2-secret"
	old := legacyEncrypt(t, plaintext, testKey)

	// 用户明确要求不加版本位、不做兼容：旧串因 salt/nonce 切分错位而通不过
	// GCM 认证标签，必须报错且不得返回任何内容。
	got, err := DecryptPassword(old, testKey)
	if err == nil {
		t.Fatalf("旧布局密文竟然可解，与不兼容的口径矛盾: %q", got)
	}
	if got != "" {
		t.Errorf("拒绝解密时仍返回了内容: %q", got)
	}
	if strings.Contains(got, plaintext) {
		t.Errorf("旧明文被还原: %q", got)
	}
}

func TestEmptyKeyIsRejected(t *testing.T) {
	if _, err := EncryptPassword("x", ""); err == nil {
		t.Error("空密钥竟然加密成功")
	}
	blob, err := EncryptPassword("x", testKey)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := DecryptPassword(blob, ""); err == nil {
		t.Error("空密钥竟然解密成功")
	}
}

func TestResolvePassword(t *testing.T) {
	t.Run("明文原样返回", func(t *testing.T) {
		got, err := ResolvePassword("plain-pass")
		if err != nil || got != "plain-pass" {
			t.Errorf("got %q err %v", got, err)
		}
	})

	t.Run("空串原样返回", func(t *testing.T) {
		got, err := ResolvePassword("")
		if err != nil || got != "" {
			t.Errorf("got %q err %v", got, err)
		}
	})

	t.Run("密文按 IMEX_KEY 解密", func(t *testing.T) {
		blob, err := EncryptPassword("live-pass", testKey)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		t.Setenv(EnvKey, testKey)
		got, err := ResolvePassword(blob)
		if err != nil || got != "live-pass" {
			t.Errorf("got %q err %v", got, err)
		}
	})

	t.Run("缺少 IMEX_KEY 报错", func(t *testing.T) {
		t.Setenv(EnvKey, "")
		_, err := ResolvePassword(EncryptedPasswordPrefix + "AAAA")
		if err == nil || !strings.Contains(err.Error(), EnvKey) {
			t.Errorf("期望报出 %s 未设置, got %v", EnvKey, err)
		}
	})
}
