package pipkin

import "errors"

var errCredentialMissing = errors.New("existing sign-in not found")
var errCredentialLocked = errors.New("OS credential store requires permission; silent access is unavailable")
var errCredentialUnavailable = errors.New("OS credential store is unavailable")
var errCredentialInvalid = errors.New("credential data is invalid")

const maxCredentialSize = 1 << 20
