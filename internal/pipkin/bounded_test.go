package pipkin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingBoundedReader struct{ err error }

func (reader failingBoundedReader) Read([]byte) (int, error) { return 0, reader.err }

func TestReadBounded(t *testing.T) {
	for _, size := range []int{0, 4, 5} {
		data, err := readBounded(strings.NewReader(strings.Repeat("x", size)), 4)
		if size <= 4 {
			if err != nil || len(data) != size {
				t.Fatalf("read %d bytes: got %d, %v", size, len(data), err)
			}
		} else if !errors.Is(err, errReadTooLarge) || data != nil {
			t.Fatalf("oversized input returned data: %v", err)
		}
	}
	want := errors.New("reader failed")
	if data, err := readBounded(failingBoundedReader{want}, 4); data != nil || !errors.Is(err, want) {
		t.Fatalf("reader error lost: %v", err)
	}
	if _, err := readBounded(strings.NewReader(""), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded(strings.NewReader("x"), 0); !errors.Is(err, errReadTooLarge) {
		t.Fatal("zero limit accepted a byte")
	}
}

func TestCredentialFileLimits(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int
		read  func(string) ([]byte, error)
	}{
		{"codex", 1 << 20, readCodexAuthFile},
		{"claude", 4 << 20, readClaudeFile},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials")
			for _, size := range []int{test.limit, test.limit + 1} {
				if err := os.WriteFile(path, []byte(strings.Repeat("private-content", size/15+1)[:size]), 0600); err != nil {
					t.Fatal(err)
				}
				data, err := test.read(path)
				if size == test.limit {
					if err != nil || len(data) != size {
						t.Fatalf("boundary rejected: %v", err)
					}
				} else if err == nil || data != nil || strings.Contains(err.Error(), "private-content") {
					t.Fatal("oversized credential must fail without returning content")
				}
			}
		})
	}
}
