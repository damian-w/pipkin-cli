package pipkin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
)

func claudeDecryptor(ctx context.Context, dir string) (func([]byte) ([]byte, error), error) {
	var gcm cipher.AEAD
	return func(data []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !bytes.HasPrefix(data, []byte("v10")) {
			if bytes.HasPrefix(data, []byte("v20")) {
				return nil, errors.New("Claude Desktop uses app-bound encryption that cannot be read silently")
			}
			return unprotectData(data)
		}
		if gcm == nil {
			raw, err := readFileBounded(filepath.Join(dir, "Local State"), maxClaudeFileSize)
			if err != nil {
				return nil, errors.New("Claude Desktop encryption key was not found")
			}
			var state struct {
				Crypto struct {
					Key string `json:"encrypted_key"`
				} `json:"os_crypt"`
			}
			if json.Unmarshal(raw, &state) != nil {
				return nil, errors.New("Claude Desktop encryption metadata is invalid")
			}
			wrapped, err := base64.StdEncoding.DecodeString(state.Crypto.Key)
			if err != nil || !bytes.HasPrefix(wrapped, []byte("DPAPI")) {
				return nil, errors.New("unsupported Claude Desktop encryption key")
			}
			key, err := unprotectData(wrapped[5:])
			if err != nil {
				return nil, err
			}
			block, err := aes.NewCipher(key)
			if err != nil {
				return nil, errors.New("invalid Claude Desktop encryption key")
			}
			gcm, err = cipher.NewGCM(block)
			if err != nil {
				return nil, err
			}
		}
		if len(data) < 3+gcm.NonceSize()+gcm.Overhead() {
			return nil, errors.New("invalid Claude Desktop encrypted data")
		}
		result, err := gcm.Open(nil, data[3:3+gcm.NonceSize()], data[3+gcm.NonceSize():], nil)
		if err != nil {
			return nil, errors.New("Claude Desktop credentials could not be decrypted")
		}
		return result, nil
	}, nil
}
