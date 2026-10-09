package pipkin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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

func readFileBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readBounded(file, limit)
}

func copyBounded(writer io.Writer, reader io.Reader, limit int64) error {
	n, err := io.Copy(writer, io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%w (%d bytes)", errReadTooLarge, limit)
	}
	return nil
}

type httpStatusError struct {
	URL    string
	Code   int
	Status string
}

func (e *httpStatusError) Error() string { return e.URL + " returned " + e.Status }

// download copies an HTTP 200 response body to writer, refusing more than limit bytes.
func download(client *http.Client, request *http.Request, writer io.Writer, limit int64) error {
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &httpStatusError{URL: request.URL.String(), Code: response.StatusCode, Status: response.Status}
	}
	if response.ContentLength > limit {
		return fmt.Errorf("%w (%d bytes)", errReadTooLarge, limit)
	}
	return copyBounded(writer, response.Body, limit)
}

// cappedBuffer keeps at most limit bytes of process output and records any excess.
type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	n := len(data)
	if remaining := b.limit - b.Len(); len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	b.Buffer.Write(data)
	return n, nil
}
