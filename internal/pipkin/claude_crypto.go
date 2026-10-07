package pipkin

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"errors"
)

func claudeCBCKey(password []byte, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha1.New, string(password), []byte("saltysalt"), iterations, 16)
}

func decryptClaudeCBC(encrypted, key []byte) ([]byte, error) {
	if len(encrypted) < 19 || (string(encrypted[:3]) != "v10" && string(encrypted[:3]) != "v11") || (len(encrypted)-3)%aes.BlockSize != 0 {
		return nil, errors.New("unsupported Claude Desktop encryption format")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("invalid Claude Desktop encryption key")
	}
	plaintext := append([]byte(nil), encrypted[3:]...)
	cipher.NewCBCDecrypter(block, bytes.Repeat([]byte{' '}, aes.BlockSize)).CryptBlocks(plaintext, plaintext)
	padding := int(plaintext[len(plaintext)-1])
	if padding < 1 || padding > aes.BlockSize || !bytes.Equal(plaintext[len(plaintext)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
		return nil, errors.New("Claude Desktop credentials could not be decrypted")
	}
	return plaintext[:len(plaintext)-padding], nil
}
