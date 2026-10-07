package pipkin

import "context"

func claudeDecryptor(ctx context.Context, dir string) (func([]byte) ([]byte, error), error) {
	var password []byte
	var err error
	for _, account := range []string{"Claude Key", "Claude"} {
		password, err = readGenericPassword(ctx, "Claude Safe Storage", account)
		if err == nil {
			break
		}
		if err != errCredentialMissing {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	key, err := claudeCBCKey(password, 1003)
	if err != nil {
		return nil, err
	}
	return func(data []byte) ([]byte, error) { return decryptClaudeCBC(data, key) }, nil
}
