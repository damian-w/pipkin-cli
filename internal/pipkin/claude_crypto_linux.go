package pipkin

import (
	"context"
	"errors"
)

func claudeDecryptor(ctx context.Context, dir string) (func([]byte) ([]byte, error), error) {
	basic, _ := claudeCBCKey([]byte("peanuts"), 1)
	var key []byte
	return func(data []byte) ([]byte, error) {
		if len(data) < 3 {
			return nil, errors.New("invalid Claude Desktop encrypted data")
		}
		if string(data[:3]) == "v10" {
			return decryptClaudeCBC(data, basic)
		}
		if string(data[:3]) != "v11" {
			return nil, errors.New("unsupported Claude Desktop encryption format")
		}
		if key == nil {
			var password []byte
			var err error
			for _, application := range []string{"Claude", "claude"} {
				password, err = readSecretService(ctx, map[string]string{"application": application})
				if err == nil {
					break
				}
				if err != errCredentialMissing {
					return nil, err
				}
			}
			if err != nil {
				return nil, errors.New("Claude Desktop key is unavailable in an unlocked Secret Service store")
			}
			key, err = claudeCBCKey(password, 1)
			if err != nil {
				return nil, err
			}
		}
		return decryptClaudeCBC(data, key)
	}, nil
}
