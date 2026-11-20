package backup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"errors"
)

// Encryption compatible with `openssl enc -aes-256-cbc -pbkdf2 -salt -md sha256` (what the Python
// version shells out to): "Salted__" + 8-byte salt, then AES-256-CBC with PKCS#7 padding; key and
// IV come from PBKDF2-HMAC-SHA256 with 10000 iterations.

const (
	opensslMagic      = "Salted__"
	opensslIterations = 10000
)

func deriveKeyIV(passphrase string, salt []byte) ([]byte, []byte, error) {
	material, err := pbkdf2.Key(sha256.New, passphrase, salt, opensslIterations, 48)
	if err != nil {
		return nil, nil, err
	}
	return material[:32], material[32:], nil
}

// Encrypt is _openssl_encrypt.
func Encrypt(data []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	key, iv, err := deriveKeyIV(passphrase, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padding := aes.BlockSize - len(data)%aes.BlockSize
	plain := append(append([]byte{}, data...), bytes.Repeat([]byte{byte(padding)}, padding)...)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, plain)
	return append(append([]byte(opensslMagic), salt...), out...), nil
}

// Decrypt is _openssl_decrypt.
func Decrypt(data []byte, passphrase string) ([]byte, error) {
	if len(data) < 16 || string(data[:8]) != opensslMagic {
		return nil, errors.New("解密备份失败：bad magic number")
	}
	key, iv, err := deriveKeyIV(passphrase, data[8:16])
	if err != nil {
		return nil, err
	}
	body := data[16:]
	if len(body) == 0 || len(body)%aes.BlockSize != 0 {
		return nil, errors.New("解密备份失败：bad decrypt")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(body))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, body)
	padding := int(out[len(out)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(out) || !bytes.Equal(out[len(out)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
		return nil, errors.New("解密备份失败：bad decrypt")
	}
	return out[:len(out)-padding], nil
}
