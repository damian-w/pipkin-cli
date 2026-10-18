package pipkin

// signedOutError is a sign-out with a user-facing message.
type signedOutError string

func (e signedOutError) Error() string { return string(e) }

func (e signedOutError) Is(target error) bool { return target == errSignedOut }
