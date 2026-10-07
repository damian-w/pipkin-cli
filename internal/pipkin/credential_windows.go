package pipkin

import (
	"context"
	"errors"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var credentialDLL = windows.NewLazySystemDLL("advapi32.dll")

type windowsCredential struct {
	Flags, Type             uint32
	TargetName, Comment     *uint16
	LastWritten             windows.Filetime
	BlobSize                uint32
	Blob                    *byte
	Persist, AttributeCount uint32
	Attributes              uintptr
	TargetAlias, UserName   *uint16
}

func readGenericPassword(ctx context.Context, service, account string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := windows.UTF16PtrFromString(account + "." + service)
	if err != nil {
		return nil, errCredentialInvalid
	}
	var credential *windowsCredential
	ok, _, callErr := credentialDLL.NewProc("CredReadW").Call(uintptr(unsafe.Pointer(target)), 1, 0, uintptr(unsafe.Pointer(&credential)))
	if ok == 0 {
		return nil, windowsCredentialError(ctx, callErr)
	}
	if credential != nil {
		defer credentialDLL.NewProc("CredFree").Call(uintptr(unsafe.Pointer(credential)))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return decodeWindowsCredential(credential)
}

func windowsCredentialError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch {
	case errors.Is(err, windows.ERROR_NOT_FOUND):
		return errCredentialMissing
	case errors.Is(err, windows.ERROR_ACCESS_DENIED), errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD), errors.Is(err, windows.ERROR_NO_SUCH_LOGON_SESSION):
		return errCredentialLocked
	default:
		return errCredentialUnavailable
	}
}

func decodeWindowsCredential(credential *windowsCredential) ([]byte, error) {
	if credential == nil || credential.Blob == nil || credential.BlobSize == 0 || credential.BlobSize > maxCredentialSize || credential.BlobSize%2 != 0 {
		return nil, errCredentialInvalid
	}
	units := unsafe.Slice((*uint16)(unsafe.Pointer(credential.Blob)), int(credential.BlobSize)/2)
	for i := 0; i < len(units); i++ {
		if !utf16.IsSurrogate(rune(units[i])) {
			continue
		}
		if units[i] < 0xd800 || units[i] > 0xdbff || i+1 == len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
			return nil, errCredentialInvalid
		}
		i++
	}
	return []byte(string(utf16.Decode(units))), nil
}

func unprotectData(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty encrypted data")
	}
	input := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, errCredentialLocked
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
	if output.Data == nil || output.Size == 0 || output.Size > maxCredentialSize {
		return nil, errCredentialInvalid
	}
	return append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...), nil
}
