package pipkin

import (
	"context"
	"io"
	"testing"
)

type statusUnidentifiedPort struct {
	writes int
	closed bool
}

func (p *statusUnidentifiedPort) Write(context.Context, []byte) error {
	p.writes++
	return io.ErrClosedPipe
}
func (p *statusUnidentifiedPort) ReadAvailable() ([]byte, error) { return nil, nil }
func (p *statusUnidentifiedPort) Close() error                   { p.closed = true; return nil }

func TestStatusDiscoveryClearsBusyWhenNoCandidatesRemain(t *testing.T) {
	isolate(t)
	h, _ := testHelper()
	h.device = map[string]string{"error": "old-port: resource busy"}
	h.connectPorts(context.Background(), nil, func(string) (serialPort, error) {
		t.Fatal("empty discovery attempted to open a port")
		return nil, io.ErrClosedPipe
	})
	if h.device["error"] != "" || h.conn != nil || h.device["port"] != "" {
		t.Fatalf("unplugged display retained a busy/connected claim: %#v", h.device)
	}
}

func TestStatusDiscoveryOpenUnidentifiedPortClearsOldBusy(t *testing.T) {
	isolate(t)
	h, lines := testHelper()
	h.device = map[string]string{"error": "previous-port: resource busy"}
	port := &statusUnidentifiedPort{}
	h.connectPorts(context.Background(), []string{"unidentified-port"}, func(name string) (serialPort, error) {
		if name != "unidentified-port" {
			t.Fatal("unexpected discovery candidate")
		}
		return port, nil
	})
	if h.device["error"] != "" || h.device["port"] != "" || h.conn != nil || !port.closed || port.writes != 1 || len(*lines) != 0 {
		t.Fatalf("unidentified port inherited busy state or became connected: device=%#v closed=%t writes=%d", h.device, port.closed, port.writes)
	}
}

func TestStatusDiscoveryCancellationKeepsLastObservation(t *testing.T) {
	for _, connected := range []bool{false, true} {
		isolate(t)
		h, _ := testHelper()
		original := map[string]string{"error": "last observation: resource busy"}
		var port *fakePort
		if connected {
			original = map[string]string{"port": "last-port", "firmware": "9.8.7"}
			port = &fakePort{}
			h.conn = &connection{port: port, name: "last-port"}
		}
		h.device = original
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h.connectPorts(ctx, nil, func(string) (serialPort, error) {
			t.Fatal("cancelled discovery opened a port")
			return nil, io.ErrClosedPipe
		})
		if connected {
			if h.conn == nil || h.device["port"] != "last-port" || port.closed {
				t.Fatal("cancelled discovery claimed a disconnect")
			}
		} else if h.device["error"] == "" {
			t.Fatal("cancelled discovery erased an observation without checking candidates")
		}
	}
}
