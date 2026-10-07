package pipkin

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsCredentialError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"missing", windows.ERROR_NOT_FOUND, errCredentialMissing},
		{"wrapped missing", fmt.Errorf("wrapped: %w", windows.ERROR_NOT_FOUND), errCredentialMissing},
		{"access denied", windows.ERROR_ACCESS_DENIED, errCredentialLocked},
		{"privilege required", windows.ERROR_PRIVILEGE_NOT_HELD, errCredentialLocked},
		{"no logon session", windows.ERROR_NO_SUCH_LOGON_SESSION, errCredentialLocked},
		{"invalid parameter", windows.ERROR_INVALID_PARAMETER, errCredentialUnavailable},
		{"no error status", syscall.Errno(0), errCredentialUnavailable},
		{"unknown failure", errors.New("sensitive-backend-detail"), errCredentialUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := windowsCredentialError(context.Background(), tc.err); !errors.Is(err, tc.want) {
				t.Fatalf("credential error = %v, want %v", err, tc.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := windowsCredentialError(ctx, windows.ERROR_NOT_FOUND); !errors.Is(err, context.Canceled) {
		t.Fatalf("credential cancellation = %v", err)
	}
}

func TestDecodeWindowsCredential(t *testing.T) {
	for _, tc := range []struct {
		name  string
		units []uint16
		want  string
	}{
		{"ASCII", utf16.Encode([]rune("fixture-secret")), "fixture-secret"},
		{"Unicode", utf16.Encode([]rune("fixture-\u00e9-\U0001f512")), "fixture-\u00e9-\U0001f512"},
		{"unpaired high surrogate", []uint16{0xd800}, ""},
		{"unpaired low surrogate", []uint16{0xdc00}, ""},
		{"high surrogate before ASCII", []uint16{0xd800, 'a'}, ""},
		{"two high surrogates", []uint16{0xd800, 0xdbff}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential := &windowsCredential{BlobSize: uint32(len(tc.units) * 2), Blob: (*byte)(unsafe.Pointer(&tc.units[0]))}
			value, err := decodeWindowsCredential(credential)
			if tc.want == "" {
				if len(value) != 0 || !errors.Is(err, errCredentialInvalid) {
					t.Fatalf("invalid credential result = %v", err)
				}
			} else if err != nil || string(value) != tc.want {
				t.Fatal("valid credential did not decode")
			}
		})
	}
	var unit uint16 = 'a'
	blob := (*byte)(unsafe.Pointer(&unit))
	for _, credential := range []*windowsCredential{
		nil,
		{},
		{BlobSize: 2},
		{Blob: blob},
		{BlobSize: 1, Blob: blob},
		{BlobSize: maxCredentialSize + 2, Blob: blob},
	} {
		if value, err := decodeWindowsCredential(credential); len(value) != 0 || !errors.Is(err, errCredentialInvalid) {
			t.Fatalf("invalid credential result = %v", err)
		}
	}
	units := make([]uint16, maxCredentialSize/2)
	for i := range units {
		units[i] = 'a'
	}
	value, err := decodeWindowsCredential(&windowsCredential{BlobSize: maxCredentialSize, Blob: (*byte)(unsafe.Pointer(&units[0]))})
	if err != nil || len(value) != len(units) {
		t.Fatal("credential at the size limit did not decode")
	}
}

func TestWindowsCredentialStopsBeforeStoreAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readGenericPassword(ctx, "fixture", "account"); !errors.Is(err, context.Canceled) {
		t.Fatalf("credential cancellation = %v", err)
	}
	if _, err := readGenericPassword(context.Background(), "fixture", "account\x00invalid"); !errors.Is(err, errCredentialInvalid) {
		t.Fatalf("invalid credential target = %v", err)
	}
}
