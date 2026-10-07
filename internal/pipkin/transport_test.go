package pipkin

import (
	"context"
	"strings"
	"testing"
	"time"
)

const identityLine = "v=1 kind=identity product=pipkin firmware=0.1.0 chip=esp32 board=sample hardware=unconfirmed protocol_min=1 protocol_max=1 seq=8 clock_epoch=0 unix=1800000000"

func TestIdentityRequiresValidBoundedRecord(t *testing.T) {
	if !isIdentity(parseFields(identityLine)) {
		t.Fatal("the firmware's v1 identity shape must be accepted")
	}
	invalid := []string{
		strings.Replace(identityLine, "v=1", "v=2", 1),
		identityLine + " seq=9",
		identityLine + " stray",
		identityLine + " unknown=value",
		" " + identityLine,
		identityLine + " ",
		strings.Replace(identityLine, " kind=", "  kind=", 1),
		strings.Replace(identityLine, " kind=", "\tkind=", 1),
		strings.Replace(identityLine, "seq=8", "seq=+8", 1),
		strings.Replace(identityLine, "seq=8", "seq=18446744073709551616", 1),
		strings.Replace(identityLine, "clock_epoch=0", "clock_epoch=9", 1),
		strings.Replace(identityLine, "clock_epoch=0 ", "", 1),
		strings.Replace(identityLine, "protocol_min=1", "protocol_min=2", 1),
		strings.Replace(identityLine, "protocol_max=1", "protocol_max=0", 1),
		strings.Replace(identityLine, "unix=1800000000", "unix=1", 1),
		strings.Replace(identityLine, "unix=1800000000", "unix=", 1),
		identityLine + strings.Repeat(" x=y", 33),
		identityLine + " firmware=" + strings.Repeat("x", maxPacketBytes),
	}
	for _, line := range invalid {
		if isIdentity(parseFields(line)) {
			t.Errorf("accepted malformed identity %q", line)
		}
	}
	if !isIdentity(parseFields(strings.Replace(identityLine, "unix=1800000000", "unix=null", 1))) {
		t.Fatal("an uninitialized device clock must remain valid")
	}
}

func TestFramingDiscardsOversizedRecordAcrossChunks(t *testing.T) {
	for _, chunkSize := range []int{1, 17, 1024, 10000} {
		p := &fakePort{}
		c := &connection{port: p}
		stream := strings.Repeat("x", maxPacketBytes+1000) + " " + identityLine + "\r\n" + identityLine + "\r\n"
		var got []string
		for len(stream) > 0 {
			end := min(chunkSize, len(stream))
			p.incoming, stream = []byte(stream[:end]), stream[end:]
			lines, err := c.lines()
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, lines...)
			if len(c.buffer) > maxPacketBytes+1 {
				t.Fatal("framing retained an oversized record")
			}
		}
		if len(got) != 1 || got[0] != identityLine {
			t.Fatalf("chunk size %d: oversized tail became a record: %q", chunkSize, got)
		}
	}
}

func TestFramingAcceptsMaximumPayloadWithCRLF(t *testing.T) {
	p := &fakePort{incoming: []byte(strings.Repeat("x", maxPacketBytes) + "\r\n" + strings.Repeat("x", maxPacketBytes+1) + "\n")}
	lines, err := (&connection{port: p}).lines()
	if err != nil || len(lines) != 1 || len(lines[0]) != maxPacketBytes {
		t.Fatalf("payload boundary was not respected: lengths %d, error %v", len(lines), err)
	}
}

func TestIdentifyCancellationInterruptsRetryWait(t *testing.T) {
	p := &fakePort{}
	ctx, cancel := context.WithCancel(context.Background())
	wrapped := &cancelOnWritePort{fakePort: p, cancel: cancel}
	start := time.Now()
	if result := identify(ctx, &connection{port: wrapped}, time.Minute); result != nil {
		t.Fatal("canceled identity must not connect a display")
	}
	if time.Since(start) > time.Second || len(p.written) != 1 {
		t.Fatal("cancellation did not stop identity retries promptly")
	}
}

type cancelOnWritePort struct {
	*fakePort
	cancel context.CancelFunc
}

func (p *cancelOnWritePort) Write(ctx context.Context, data []byte) error {
	err := p.fakePort.Write(ctx, data)
	p.cancel()
	return err
}
