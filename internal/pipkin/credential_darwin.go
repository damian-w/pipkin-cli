package pipkin

import (
	"context"
	"errors"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

var keychainAPI struct {
	sync.Once
	mutex                sync.Mutex
	err                  error
	security, foundation uintptr
	stringCreate         func(uintptr, string, uint32) uintptr
	dictionaryCreate     func(uintptr, int64, uintptr, uintptr) uintptr
	dictionarySet        func(uintptr, uintptr, uintptr)
	release              func(uintptr)
	copyMatching         func(uintptr, *uintptr) int32
	interactionAllowed   func(uint8) int32
	dataLength           func(uintptr) int64
	dataBytes            func(uintptr) *byte
}

func readGenericPassword(ctx context.Context, service, account string) ([]byte, error) {
	return readGenericPasswordMode(ctx, service, account, false)
}

func readGenericPasswordMode(ctx context.Context, service, account string, allowUI bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a := &keychainAPI
	// Keychain UI policy is process-wide; serialize foreground and background reads.
	a.mutex.Lock()
	defer a.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.Do(func() {
		a.foundation, a.err = purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if a.err != nil {
			return
		}
		a.security, a.err = purego.Dlopen("/System/Library/Frameworks/Security.framework/Security", purego.RTLD_NOW|purego.RTLD_LOCAL)
		if a.err != nil {
			return
		}
		purego.RegisterLibFunc(&a.stringCreate, a.foundation, "CFStringCreateWithCString")
		purego.RegisterLibFunc(&a.dictionaryCreate, a.foundation, "CFDictionaryCreateMutable")
		purego.RegisterLibFunc(&a.dictionarySet, a.foundation, "CFDictionarySetValue")
		purego.RegisterLibFunc(&a.release, a.foundation, "CFRelease")
		purego.RegisterLibFunc(&a.dataLength, a.foundation, "CFDataGetLength")
		purego.RegisterLibFunc(&a.dataBytes, a.foundation, "CFDataGetBytePtr")
		purego.RegisterLibFunc(&a.copyMatching, a.security, "SecItemCopyMatching")
		purego.RegisterLibFunc(&a.interactionAllowed, a.security, "SecKeychainSetUserInteractionAllowed")
		if a.interactionAllowed(0) != 0 {
			a.err = errCredentialLocked
		}
	})
	if a.err != nil {
		return nil, errors.New("macOS credential store is unavailable")
	}
	if allowUI {
		if a.interactionAllowed(1) != 0 {
			return nil, errCredentialLocked
		}
		defer a.interactionAllowed(0)
	}
	constantError := false
	constant := func(lib uintptr, name string) uintptr {
		address, err := purego.Dlsym(lib, name)
		if err != nil {
			constantError = true
			return 0
		}
		// Framework-owned memory: preserve the foreign pointer without arithmetic.
		pointer := *(*unsafe.Pointer)(unsafe.Pointer(&address))
		value := *(*uintptr)(pointer)
		if value == 0 {
			constantError = true
		}
		return value
	}
	query := a.dictionaryCreate(0, 0, 0, 0)
	if query == 0 {
		return nil, errCredentialLocked
	}
	defer a.release(query)
	set := func(key string, value uintptr) {
		keyValue := constant(a.security, key)
		if keyValue != 0 && value != 0 {
			a.dictionarySet(query, keyValue, value)
		}
	}
	set("kSecClass", constant(a.security, "kSecClassGenericPassword"))
	set("kSecMatchLimit", constant(a.security, "kSecMatchLimitOne"))
	set("kSecReturnData", constant(a.foundation, "kCFBooleanTrue"))
	// Background reads fail instead of opening a Keychain permission or unlock dialog.
	uiPolicy := "kSecUseAuthenticationUIFail"
	if allowUI {
		uiPolicy = "kSecUseAuthenticationUIAllow"
	}
	set("kSecUseAuthenticationUI", constant(a.security, uiPolicy))
	for key, value := range map[string]string{"kSecAttrService": service, "kSecAttrAccount": account} {
		text := a.stringCreate(0, value, 0x08000100)
		if text == 0 {
			return nil, errCredentialMissing
		}
		defer a.release(text)
		set(key, text)
	}
	if constantError {
		return nil, errors.New("macOS credential APIs are unavailable")
	}
	var data uintptr
	status := a.copyMatching(query, &data)
	if status == -25300 {
		return nil, errCredentialMissing
	}
	if status != 0 || data == 0 {
		return nil, errCredentialLocked
	}
	defer a.release(data)
	length := a.dataLength(data)
	if length <= 0 || length > maxCredentialSize {
		return nil, errCredentialInvalid
	}
	pointer := a.dataBytes(data)
	if pointer == nil {
		return nil, errCredentialInvalid
	}
	return append([]byte(nil), unsafe.Slice(pointer, int(length))...), nil
}
