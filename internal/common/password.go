package common

import (
	"crypto/aes"
	"crypto/cipher"
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
	EnvKey = "GOMYDUMP_KEY"
)

// IsEncryptedPassword 判断密码是否为加密格式
func IsEncryptedPassword(password string) bool {
	return strings.HasPrefix(password, EncryptedPasswordPrefix)
}

// EncryptPassword 使用 AES-GCM 加密明文密码，返回 base64 编码的密文（带 ENC: 前缀）
func EncryptPassword(plaintext string, key string) (string, error) {
	if key == "" {
		return "", errors.New("encryption key is empty")
	}

	// 使用 SHA-256 将任意长度密钥转为 32 字节 AES-256 密钥
	aesKey := deriveKey(key)

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

	// 加密
	ciphertext := aesGCM.Seal(nonce, nonce, []byte(plaintext), nil)

	// base64 编码并加前缀
	return EncryptedPasswordPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
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

	aesKey := deriveKey(key)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", errors.Wrap(err, "failed to create cipher")
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", errors.Wrap(err, "failed to create GCM")
	}

	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("failed to decrypt: wrong key or corrupted data")
	}

	return string(plaintext), nil
}

// ResolvePassword 解析密码：
// 1. 如果是 ENC: 开头，从环境变量 GOMYDUMP_KEY 获取密钥并解密
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

// deriveKey 使用 SHA-256 将任意长度密钥转为 32 字节 AES-256 密钥
func deriveKey(key string) []byte {
	hash := sha256.Sum256([]byte(key))
	return hash[:]
}
