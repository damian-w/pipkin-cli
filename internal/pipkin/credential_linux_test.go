package pipkin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

const (
	secretServiceTestSearch = "org.freedesktop.Secret.Service.SearchItems"
	secretServiceTestOpen   = "org.freedesktop.Secret.Service.OpenSession"
	secretServiceTestGet    = "org.freedesktop.Secret.Item.GetSecret"
	secretServiceTestClose  = "org.freedesktop.Secret.Session.Close"
)

type secretServiceTestConnection struct {
	t         *testing.T
	replies   map[string]*dbus.Call
	calls     []string
	afterCall func(string)
	closed    bool
}

func (c *secretServiceTestConnection) Object(destination string, path dbus.ObjectPath) dbus.BusObject {
	if destination != "org.freedesktop.secrets" {
		c.t.Fatal("unexpected credential store destination")
	}
	return secretServiceTestObject{conn: c, path: path}
}

func (c *secretServiceTestConnection) Close() error {
	c.closed = true
	return nil
}

type secretServiceTestObject struct {
	dbus.BusObject
	conn *secretServiceTestConnection
	path dbus.ObjectPath
}

func (o secretServiceTestObject) CallWithContext(ctx context.Context, method string, flags dbus.Flags, args ...any) *dbus.Call {
	c := o.conn
	c.calls = append(c.calls, method)
	switch method {
	case secretServiceTestSearch:
		if o.path != "/org/freedesktop/secrets" || flags != 0 || !reflect.DeepEqual(args, []any{map[string]string{"service": "fixture", "username": "account"}}) {
			c.t.Fatal("unexpected credential search")
		}
	case secretServiceTestOpen:
		if o.path != "/org/freedesktop/secrets" || flags != 0 || !reflect.DeepEqual(args, []any{"plain", dbus.MakeVariant("")}) {
			c.t.Fatal("unexpected credential session request")
		}
	case secretServiceTestGet:
		if o.path != "/item" || flags != 0 || !reflect.DeepEqual(args, []any{dbus.ObjectPath("/session")}) {
			c.t.Fatal("unexpected credential read")
		}
	case secretServiceTestClose:
		if o.path != "/session" || flags != dbus.FlagNoReplyExpected || len(args) != 0 {
			c.t.Fatal("unexpected credential session cleanup")
		}
	default:
		c.t.Fatal("unexpected credential store method")
	}
	if c.afterCall != nil {
		c.afterCall(method)
	}
	if reply, ok := c.replies[method]; ok {
		return reply
	}
	switch method {
	case secretServiceTestSearch:
		return &dbus.Call{Body: []any{[]dbus.ObjectPath{"/item"}, []dbus.ObjectPath{}}}
	case secretServiceTestOpen:
		return &dbus.Call{Body: []any{dbus.MakeVariant(""), dbus.ObjectPath("/session")}}
	case secretServiceTestGet:
		return secretServiceTestSecret("/session", nil, []byte("fixture-secret"))
	default:
		return &dbus.Call{}
	}
}

func secretServiceTestSecret(session dbus.ObjectPath, parameters, value []byte) *dbus.Call {
	return &dbus.Call{Body: []any{[]any{session, parameters, value, "text/plain"}}}
}

func TestSecretServiceReadOutcomes(t *testing.T) {
	searchOnly := []string{secretServiceTestSearch}
	openOnly := []string{secretServiceTestSearch, secretServiceTestOpen}
	fullRead := []string{secretServiceTestSearch, secretServiceTestOpen, secretServiceTestGet, secretServiceTestClose}
	backendDetail := "sensitive-backend-detail"
	for _, tc := range []struct {
		name      string
		replies   map[string]*dbus.Call
		wantErr   error
		wantText  string
		wantValue []byte
		wantCalls []string
	}{
		{name: "valid", wantValue: []byte("fixture-secret"), wantCalls: fullRead},
		{name: "size limit", replies: map[string]*dbus.Call{secretServiceTestGet: secretServiceTestSecret("/session", nil, bytes.Repeat([]byte("a"), maxCredentialSize))}, wantValue: bytes.Repeat([]byte("a"), maxCredentialSize), wantCalls: fullRead},
		{name: "no matches", replies: map[string]*dbus.Call{secretServiceTestSearch: {Body: []any{[]dbus.ObjectPath{}, []dbus.ObjectPath{}}}}, wantErr: errCredentialMissing, wantCalls: searchOnly},
		{name: "locked match", replies: map[string]*dbus.Call{secretServiceTestSearch: {Body: []any{[]dbus.ObjectPath{}, []dbus.ObjectPath{"/locked"}}}}, wantErr: errCredentialLocked, wantCalls: searchOnly},
		{name: "ambiguous unlocked", replies: map[string]*dbus.Call{secretServiceTestSearch: {Body: []any{[]dbus.ObjectPath{"/item", "/other"}, []dbus.ObjectPath{}}}}, wantText: "ambiguous matching sign-ins", wantCalls: searchOnly},
		{name: "ambiguous mixed", replies: map[string]*dbus.Call{secretServiceTestSearch: {Body: []any{[]dbus.ObjectPath{"/item"}, []dbus.ObjectPath{"/locked"}}}}, wantText: "ambiguous matching sign-ins", wantCalls: searchOnly},
		{name: "ambiguous locked", replies: map[string]*dbus.Call{secretServiceTestSearch: {Body: []any{[]dbus.ObjectPath{}, []dbus.ObjectPath{"/locked", "/other"}}}}, wantText: "ambiguous matching sign-ins", wantCalls: searchOnly},
		{name: "invalid item path", replies: map[string]*dbus.Call{secretServiceTestSearch: {Body: []any{[]dbus.ObjectPath{"invalid"}, []dbus.ObjectPath{}}}}, wantErr: errCredentialInvalid, wantCalls: searchOnly},
		{name: "search unavailable", replies: map[string]*dbus.Call{secretServiceTestSearch: {Err: dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown", Body: []any{backendDetail}}}}, wantErr: errCredentialUnavailable, wantCalls: searchOnly},
		{name: "search denied", replies: map[string]*dbus.Call{secretServiceTestSearch: {Err: dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{backendDetail}}}}, wantErr: errCredentialLocked, wantCalls: searchOnly},
		{name: "malformed search", replies: map[string]*dbus.Call{secretServiceTestSearch: {Body: []any{backendDetail}}}, wantErr: errCredentialUnavailable, wantCalls: searchOnly},
		{name: "session unavailable", replies: map[string]*dbus.Call{secretServiceTestOpen: {Err: errors.New(backendDetail)}}, wantErr: errCredentialUnavailable, wantCalls: openOnly},
		{name: "session denied", replies: map[string]*dbus.Call{secretServiceTestOpen: {Err: dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{backendDetail}}}}, wantErr: errCredentialLocked, wantCalls: openOnly},
		{name: "malformed session", replies: map[string]*dbus.Call{secretServiceTestOpen: {Body: []any{backendDetail}}}, wantErr: errCredentialUnavailable, wantCalls: openOnly},
		{name: "invalid session path", replies: map[string]*dbus.Call{secretServiceTestOpen: {Body: []any{dbus.MakeVariant(""), dbus.ObjectPath("invalid")}}}, wantErr: errCredentialInvalid, wantCalls: openOnly},
		{name: "secret unavailable", replies: map[string]*dbus.Call{secretServiceTestGet: {Err: errors.New(backendDetail)}}, wantErr: errCredentialUnavailable, wantCalls: fullRead},
		{name: "secret locked", replies: map[string]*dbus.Call{secretServiceTestGet: {Err: dbus.Error{Name: "org.freedesktop.Secret.Error.IsLocked", Body: []any{backendDetail}}}}, wantErr: errCredentialLocked, wantCalls: fullRead},
		{name: "malformed secret", replies: map[string]*dbus.Call{secretServiceTestGet: {Body: []any{backendDetail}}}, wantErr: errCredentialUnavailable, wantCalls: fullRead},
		{name: "empty secret", replies: map[string]*dbus.Call{secretServiceTestGet: secretServiceTestSecret("/session", nil, nil)}, wantErr: errCredentialInvalid, wantCalls: fullRead},
		{name: "oversized secret", replies: map[string]*dbus.Call{secretServiceTestGet: secretServiceTestSecret("/session", nil, make([]byte, maxCredentialSize+1))}, wantErr: errCredentialInvalid, wantCalls: fullRead},
		{name: "wrong session", replies: map[string]*dbus.Call{secretServiceTestGet: secretServiceTestSecret("/other", nil, []byte("fixture-secret"))}, wantErr: errCredentialInvalid, wantCalls: fullRead},
		{name: "unexpected plain parameters", replies: map[string]*dbus.Call{secretServiceTestGet: secretServiceTestSecret("/session", []byte("parameters"), []byte("fixture-secret"))}, wantErr: errCredentialInvalid, wantCalls: fullRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &secretServiceTestConnection{t: t, replies: tc.replies}
			value, err := readSecretServiceWithConnection(context.Background(), map[string]string{"service": "fixture", "username": "account"}, func(context.Context) (secretServiceConnection, error) { return conn, nil })
			if tc.wantText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantText) {
					t.Fatal("credential error did not report ambiguity")
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("credential error = %v, want %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), backendDetail) {
				t.Fatal("credential error exposed backend details")
			}
			if !bytes.Equal(value, tc.wantValue) {
				t.Fatal("unexpected credential bytes")
			}
			if !conn.closed || !reflect.DeepEqual(conn.calls, tc.wantCalls) {
				t.Fatalf("credential cleanup or call sequence changed: %v", conn.calls)
			}
		})
	}
}

func TestSecretServiceConnectionErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"unavailable", errors.New("sensitive-backend-detail"), errCredentialUnavailable},
		{"access denied", dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"sensitive-backend-detail"}}, errCredentialLocked},
		{"connection permission", fmt.Errorf("wrapped: %w", fs.ErrPermission), errCredentialLocked},
		{"authentication failed", dbus.NewError("org.freedesktop.DBus.Error.AuthFailed", []any{"sensitive-backend-detail"}), errCredentialLocked},
		{"authorization required", fmt.Errorf("wrapped: %w", dbus.Error{Name: "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired"}), errCredentialLocked},
		{"canceled", fmt.Errorf("wrapped: %w", context.Canceled), context.Canceled},
		{"deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := readSecretServiceWithConnection(context.Background(), nil, func(context.Context) (secretServiceConnection, error) { return nil, tc.err })
			if len(value) != 0 || !errors.Is(err, tc.want) {
				t.Fatalf("credential error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSecretServiceCancellation(t *testing.T) {
	for _, stage := range []string{"before connection", "connection", secretServiceTestSearch, secretServiceTestOpen, secretServiceTestGet} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn := &secretServiceTestConnection{t: t, afterCall: func(method string) {
				if method == stage {
					cancel()
				}
			}}
			connected := false
			if stage == "before connection" {
				cancel()
			}
			value, err := readSecretServiceWithConnection(ctx, map[string]string{"service": "fixture", "username": "account"}, func(context.Context) (secretServiceConnection, error) {
				connected = true
				if stage == "connection" {
					cancel()
				}
				return conn, nil
			})
			if len(value) != 0 || !errors.Is(err, context.Canceled) {
				t.Fatalf("credential cancellation = %v", err)
			}
			if connected != conn.closed {
				t.Fatal("credential connection was not closed after cancellation")
			}
			if stage == "before connection" && connected {
				t.Fatal("canceled read attempted a credential connection")
			}
		})
	}
}
