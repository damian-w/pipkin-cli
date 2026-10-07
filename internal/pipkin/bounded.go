package pipkin

import (
	"errors"
	"io"
)

var errReadTooLarge = errors.New("data exceeds read limit")

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errReadTooLarge
	}
	return data, nil
}
