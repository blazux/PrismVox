// Package secrets decrypts credentials written by the configuration service.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"os"
	"strings"
)

const Prefix = "enc:v1:"

func Decrypt(value, field string) (string, error) {
	if !strings.HasPrefix(value, Prefix) {
		return value, nil // Compatibility while upgrading existing installations.
	}
	path := os.Getenv("SECRET_KEY_PATH")
	if path == "" {
		path = "/var/lib/prismvox/secret.key"
	}
	key, err := os.ReadFile(path)
	if err != nil || len(key) != 32 {
		return "", errors.New("persistent secret key unavailable")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("unsafe secret key permissions")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, Prefix))
	if err != nil || len(data) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("invalid encrypted credential")
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte(field))
	if err != nil {
		return "", errors.New("stored credential authentication failed")
	}
	return string(plain), nil
}
