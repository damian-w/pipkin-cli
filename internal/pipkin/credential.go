package pipkin

import (
	"errors"
	"os"
)

var errCredentialMissing = errors.New("existing sign-in not found")
var errCredentialLocked = errors.New("OS credential store requires permission; silent access is unavailable")
var errCredentialUnavailable = errors.New("OS credential store is unavailable")
var errCredentialInvalid = errors.New("credential data is invalid")

const maxCredentialSize = 1 << 20

func readCredentialFile(path string, limit int64, failureMessage string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := readBounded(file, limit)
	if err != nil {
		return nil, errors.New(failureMessage)
	}
	return data, nil
}
