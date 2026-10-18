package pipkin

// signedOutError keeps sign-out classification separate from its message.
type signedOutError string

func (e signedOutError) Error() string { return string(e) }

func (e signedOutError) Is(target error) bool { return target == errSignedOut }
