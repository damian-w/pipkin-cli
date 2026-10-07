package pipkin

import (
	"context"
	"errors"
	"io/fs"

	"github.com/godbus/dbus/v5"
)

func readGenericPassword(ctx context.Context, service, account string) ([]byte, error) {
	return readSecretService(ctx, map[string]string{"service": service, "username": account})
}

type secretServiceConnection interface {
	Object(string, dbus.ObjectPath) dbus.BusObject
	Close() error
}

func readSecretService(ctx context.Context, attributes map[string]string) ([]byte, error) {
	return readSecretServiceWithConnection(ctx, attributes, func(ctx context.Context) (secretServiceConnection, error) {
		return dbus.ConnectSessionBus(dbus.WithContext(ctx))
	})
}

func readSecretServiceWithConnection(ctx context.Context, attributes map[string]string, connect func(context.Context) (secretServiceConnection, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := connect(ctx)
	if err != nil {
		return nil, secretServiceError(ctx, err)
	}
	defer conn.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	service := conn.Object("org.freedesktop.secrets", "/org/freedesktop/secrets")
	var unlocked, locked []dbus.ObjectPath
	if err := service.CallWithContext(ctx, "org.freedesktop.Secret.Service.SearchItems", 0, attributes).Store(&unlocked, &locked); err != nil {
		return nil, secretServiceError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(unlocked)+len(locked) > 1 {
		return nil, errors.New("credential store has ambiguous matching sign-ins")
	}
	if len(unlocked) == 0 {
		if len(locked) > 0 {
			return nil, errCredentialLocked
		}
		return nil, errCredentialMissing
	}
	if !unlocked[0].IsValid() {
		return nil, errCredentialInvalid
	}
	var output dbus.Variant
	var session dbus.ObjectPath
	if err := service.CallWithContext(ctx, "org.freedesktop.Secret.Service.OpenSession", 0, "plain", dbus.MakeVariant("")).Store(&output, &session); err != nil {
		return nil, secretServiceError(ctx, err)
	}
	if !session.IsValid() {
		return nil, errCredentialInvalid
	}
	defer conn.Object("org.freedesktop.secrets", session).CallWithContext(ctx, "org.freedesktop.Secret.Session.Close", dbus.FlagNoReplyExpected)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var secret struct {
		Session     dbus.ObjectPath
		Parameters  []byte
		Value       []byte
		ContentType string
	}
	item := conn.Object("org.freedesktop.secrets", unlocked[0])
	if err := item.CallWithContext(ctx, "org.freedesktop.Secret.Item.GetSecret", 0, session).Store(&secret); err != nil {
		return nil, secretServiceError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if secret.Session != session || len(secret.Parameters) != 0 || len(secret.Value) == 0 || len(secret.Value) > maxCredentialSize {
		return nil, errCredentialInvalid
	}
	return secret.Value, nil
}

func secretServiceError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, fs.ErrPermission) {
		return errCredentialLocked
	}
	var busError dbus.Error
	var busErrorPointer *dbus.Error
	var name string
	switch {
	case errors.As(err, &busError):
		name = busError.Name
	case errors.As(err, &busErrorPointer):
		name = busErrorPointer.Name
	}
	switch name {
	case "org.freedesktop.Secret.Error.IsLocked", "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.AuthFailed", "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired":
		return errCredentialLocked
	default:
		return errCredentialUnavailable
	}
}
