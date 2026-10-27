package common

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"github.com/pkg/errors"
	"io"
	"os"
	"strings"
)

const (
	// EncryptedPasswordPrefix 加密密码前缀，用于区分明文和密文
	EncryptedPasswordPrefix = "ENC:"
	// EnvKey 密钥环境变量名
	EnvKey = "IMEX_KEY"

	// kdfSaltSize 是 PBKDF2 盐的字节数，每次加密随机生成并前置进密文。
	// 没有盐时同一个 IMEX_KEY 下所有密文共用同一个派生密钥，一次离线破解
	// 就能解开通用同一把密钥的所有口令。
	kdfSaltSize = 16
	// kdfIterations 是 PBKDF2-HMAC-SHA256 的迭代次数。实测（4 核沙箱、
	// go1.26.10、单线程）600000 次迭代派生一个密钥约 310 ms，即离线猜测
	// 约 3 次/秒；作为对照，改之前"sha256(IMEX_KEY) 直接当 AES 密钥"是
	// 939,822 次/秒。每次导出/导入只在进程起始处解密一次，这点开销落在
	// 建立连接之前，不影响吞吐。
	kdfIterations = 600000
	// kdfKeySize 是派生出的 AES-256 密钥长度。
	kdfKeySize = 32
)

// IsEncryptedPassword 判断密码是否为加密格式
func IsEncryptedPassword(password string) bool {
	return strings.HasPrefix(password, EncryptedPasswordPrefix)
}

// EncryptPassword 使用 AES-256-GCM 加密明文密码，返回 base64 编码的密文（带 ENC: 前缀）。
//
// 密文布局是 base64(salt || nonce || ciphertext+tag)：salt 是 kdfSaltSize 字节的随机
// PBKDF2 盐，nonce 是 GCM 的随机 nonce，两者都前置，解密端无需额外参数即可还原。
// 密钥由 deriveKey 用 PBKDF2-HMAC-SHA256 从 (key, salt) 派生。
//
// 2026-10-26 起不再兼容旧布局（base64(nonce || ciphertext+tag)，密钥为
// sha256(IMEX_KEY)），也不引入版本标识：旧密文会因盐/nonce 切分错位而通不过
// GCM 认证标签，统一以 "wrong key or corrupted data" 失败，需要重新 encrypt。
func EncryptPassword(plaintext string, key string) (string, error) {
	if key == "" {
		return "", errors.New("encryption key is empty")
	}

	salt := make([]byte, kdfSaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", errors.Wrap(err, "failed to generate salt")
	}

	aesKey, err := deriveKey(key, salt)
	if err != nil {
		return "", errors.Wrap(err, "failed to derive key")
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", errors.Wrap(err, "failed to create cipher")
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", errors.Wrap(err, "failed to create GCM")
	}

	// 生成随机 nonce
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", errors.Wrap(err, "failed to generate nonce")
	}

	// 加密：salt 与 nonce 依次前置，解密端按 kdfSaltSize 与 GCM 的 nonce 长度切回
	blob := make([]byte, 0, len(salt)+len(nonce)+len(plaintext)+aesGCM.Overhead())
	blob = append(blob, salt...)
	blob = append(blob, nonce...)
	blob = aesGCM.Seal(blob, nonce, []byte(plaintext), nil)

	// base64 编码并加前缀
	return EncryptedPasswordPrefix + base64.StdEncoding.EncodeToString(blob), nil
}

// DecryptPassword 解密 ENC: 前缀的密码
func DecryptPassword(encrypted string, key string) (string, error) {
	if key == "" {
		return "", errors.New("encryption key is empty")
	}

	// 去掉前缀
	encoded := strings.TrimPrefix(encrypted, EncryptedPasswordPrefix)

	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.Wrap(err, "failed to decode base64")
	}

	if len(ciphertext) < kdfSaltSize {
		return "", errors.New("ciphertext too short")
	}
	salt, rest := ciphertext[:kdfSaltSize], ciphertext[kdfSaltSize:]

	aesKey, err := deriveKey(key, salt)
	if err != nil {
		return "", errors.Wrap(err, "failed to derive key")
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", errors.Wrap(err, "failed to create cipher")
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", errors.Wrap(err, "failed to create GCM")
	}

	nonceSize := aesGCM.NonceSize()
	if len(rest) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertext := rest[:nonceSize], rest[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("failed to decrypt: wrong key or corrupted data")
	}

	return string(plaintext), nil
}

// ResolvePassword 解析密码：
// 1. 如果是 ENC: 开头，从环境变量 IMEX_KEY 获取密钥并解密
// 2. 否则直接返回（明文或空）
func ResolvePassword(password string) (string, error) {
	if !IsEncryptedPassword(password) {
		return password, nil
	}

	key := os.Getenv(EnvKey)
	if key == "" {
		return "", errors.Errorf("password is encrypted but %s environment variable is not set", EnvKey)
	}

	return DecryptPassword(password, key)
}

// deriveKey 使用 PBKDF2-HMAC-SHA256 把任意长度密钥与随机盐派生为 32 字节 AES-256 密钥。
// 单个 SHA-256 不能作为这里的密钥派生：它没有工作因子，攻击者拿到一条密文就能以每秒
// 数十万次的速率离线尝试密钥候选，而 GCM 的认证标签让每一次猜测都能当场验对错。
func deriveKey(key string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, key, salt, kdfIterations, kdfKeySize)
}
