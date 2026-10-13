//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// scriptedINEngine serves bulk IN reads from a script; with the script used up
// a read blocks until AbortEndpoint, like a device with nothing to send.
type scriptedINEngine struct {
	access  sync.Mutex
	script  [][]byte
	reads   int
	aborts  []uint8
	abort   chan struct{}
	release chan struct{}
}

func newScriptedINEngine(script ...[]byte) *scriptedINEngine {
	return &scriptedINEngine{script: script, abort: make(chan struct{}, 16), release: make(chan struct{}, 16)}
}

func (e *scriptedINEngine) Submit(request URBRequest) URBResponse {
	if request.Command.Header.Endpoint == 0 {
		return URBResponse{}
	}
	for {
		e.access.Lock()
		if e.reads < len(e.script) {
			data := e.script[e.reads]
			e.reads++
			e.access.Unlock()
			n := copy(request.Buffer, data)
			return URBResponse{ActualLength: int32(n), Buffer: request.Buffer}
		}
		e.access.Unlock()
		select {
		case <-e.abort:
			return URBResponse{Status: usbipStatusECONNRESET}
		case <-e.release:
		}
	}
}

func (e *scriptedINEngine) add(data ...[]byte) {
	e.access.Lock()
	e.script = append(e.script, data...)
	e.access.Unlock()
	e.release <- struct{}{}
}

func (e *scriptedINEngine) readCount() int {
	e.access.Lock()
	defer e.access.Unlock()
	return e.reads
}

func (e *scriptedINEngine) abortedEndpoints() []uint8 {
	e.access.Lock()
	defer e.access.Unlock()
	return append([]uint8(nil), e.aborts...)
}

func (e *scriptedINEngine) AbortEndpoint(endpoint uint8) error {
	e.access.Lock()
	e.aborts = append(e.aborts, endpoint)
	e.access.Unlock()
	select {
	case e.abort <- struct{}{}:
	default:
	}
	return nil
}

func (e *scriptedINEngine) Close() error { return nil }

type readAheadClient struct {
	t    *testing.T
	conn net.Conn
	seq  uint32
}

func startReadAheadSession(t *testing.T, engine URBEngine) *readAheadClient {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	session := newUserspaceURBSession(ctx, nopLogger(), serverConn, engine).withReadAhead(serialReadAheadPolicy(ftdiVendorID, SpeedFull))
	require.NoError(t, session.Start())
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = session.Close()
		cancel()
	})
	return &readAheadClient{t: t, conn: clientConn}
}

func (c *readAheadClient) submitIN(length int32) uint32 {
	c.seq++
	require.NoError(c.t, WriteSubmitCommand(c.conn, SubmitCommand{
		Header:               DataHeader{Command: CmdSubmit, SeqNum: c.seq, DevID: 7, Direction: USBIPDirIn, Endpoint: 1},
		TransferBufferLength: length,
		NumberOfPackets:      nonIsoPacketCount,
	}))
	return c.seq
}

func (c *readAheadClient) readReply() (DataHeader, SubmitResponse, UnlinkResponse) {
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	header, err := ReadDataHeader(c.conn)
	require.NoError(c.t, err)
	if header.Command == RetUnlink {
		response, err := ReadUnlinkResponseBody(c.conn, header)
		require.NoError(c.t, err)
		return header, SubmitResponse{}, response
	}
	response, err := ReadSubmitResponseBody(c.conn, header, USBIPDirIn)
	require.NoError(c.t, err)
	return header, response, UnlinkResponse{}
}

func (c *readAheadClient) receive(length int32) SubmitResponse {
	seq := c.submitIN(length)
	header, response, _ := c.readReply()
	require.Equal(c.t, RetSubmit, header.Command)
	require.Equal(c.t, seq, header.SeqNum)
	return response
}

func TestReadAheadReadsBeforeTheClientAsks(t *testing.T) {
	engine := newScriptedINEngine([]byte{0x01, 0x60, 'a'}, []byte{0x01, 0x60, 'b'}, []byte{0x01, 0x60, 'c'})
	client := startReadAheadSession(t, engine)

	require.Equal(t, []byte{0x01, 0x60, 'a'}, client.receive(64).Buffer)
	// One client URB, yet the reader drains the device on its own.
	require.Eventually(t, func() bool { return engine.readCount() == 3 }, time.Second, 5*time.Millisecond)
	require.Equal(t, []byte{0x01, 0x60, 'b'}, client.receive(64).Buffer)
	require.Equal(t, []byte{0x01, 0x60, 'c'}, client.receive(64).Buffer)
}

func TestReadAheadCollapsesFTDIStatusOnlyTransfers(t *testing.T) {
	status := []byte{0x01, 0x60}
	engine := newScriptedINEngine([]byte{0x01, 0x60, 'x'}, status, []byte{0x01, 0x00}, status, status, []byte{0x01, 0x60, 'y'})
	client := startReadAheadSession(t, engine)

	require.Equal(t, []byte{0x01, 0x60, 'x'}, client.receive(64).Buffer)
	require.Eventually(t, func() bool { return engine.readCount() == 6 }, time.Second, 5*time.Millisecond)
	// The idle status transfers between the two data transfers left no trace
	// (0x00 and 0x60 differ only in the transmitter-empty bits).
	require.Equal(t, []byte{0x01, 0x60, 'y'}, client.receive(64).Buffer)
}

func TestReadAheadKeepsFTDIErrorStatus(t *testing.T) {
	overrun := []byte{0x01, 0x62}
	engine := newScriptedINEngine([]byte{0x01, 0x60, 'x'}, overrun, []byte{0x01, 0x60, 'y'})
	client := startReadAheadSession(t, engine)

	require.Equal(t, []byte{0x01, 0x60, 'x'}, client.receive(64).Buffer)
	require.Eventually(t, func() bool { return engine.readCount() == 3 }, time.Second, 5*time.Millisecond)
	require.Equal(t, overrun, client.receive(64).Buffer)
	require.Equal(t, []byte{0x01, 0x60, 'y'}, client.receive(64).Buffer)
}

func TestReadAheadSplitsAtAPacketBoundary(t *testing.T) {
	long := make([]byte, 128)
	for i := range long {
		long[i] = byte(i)
	}
	engine := newScriptedINEngine([]byte{0x01, 0x60}, long)
	client := startReadAheadSession(t, engine)

	// The reader reads with the length of the client's URBs (128 here) ...
	require.Equal(t, []byte{0x01, 0x60}, client.receive(128).Buffer)
	require.Eventually(t, func() bool { return engine.readCount() == 2 }, time.Second, 5*time.Millisecond)
	// ... and a later, smaller URB gets the queued transfer in two parts.
	first := client.receive(64)
	require.Equal(t, int32(0), first.Status)
	require.Equal(t, long[:64], first.Buffer)
	second := client.receive(64)
	require.Equal(t, long[64:], second.Buffer)
}

func TestReadAheadUnlinkKeepsTheDeviceReadAndTheData(t *testing.T) {
	engine := newScriptedINEngine()
	client := startReadAheadSession(t, engine)

	seq := client.submitIN(64)
	require.Eventually(t, func() bool {
		return len(engine.abortedEndpoints()) == 0 && engine.readCount() == 0
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, WriteUnlinkCommand(client.conn, UnlinkCommand{
		Header: DataHeader{Command: CmdUnlink, SeqNum: seq + 100, DevID: 7, Direction: USBIPDirIn, Endpoint: 1},
		SeqNum: seq,
	}))
	header, _, unlink := client.readReply()
	require.Equal(t, RetUnlink, header.Command)
	require.Equal(t, int32(usbipStatusECONNRESET), unlink.Status)
	require.Empty(t, engine.abortedEndpoints(), "unlinking a waiting submit must not abort the reader")

	client.seq = seq + 100
	engine.add([]byte{0x01, 0x60, 'z'})
	require.Equal(t, []byte{0x01, 0x60, 'z'}, client.receive(64).Buffer)
}

func TestReadAheadPurgeDropsQueuedData(t *testing.T) {
	engine := newScriptedINEngine([]byte{0x01, 0x60, 'a'}, []byte{0x01, 0x60, 'b'})
	client := startReadAheadSession(t, engine)

	require.Equal(t, []byte{0x01, 0x60, 'a'}, client.receive(64).Buffer)
	require.Eventually(t, func() bool { return engine.readCount() == 2 }, time.Second, 5*time.Millisecond)

	// FTDI SIO_RESET with wValue 1: purge RX.
	client.seq++
	require.NoError(t, WriteSubmitCommand(client.conn, SubmitCommand{
		Header:          DataHeader{Command: CmdSubmit, SeqNum: client.seq, DevID: 7, Direction: USBIPDirOut, Endpoint: 0},
		NumberOfPackets: nonIsoPacketCount,
		Setup:           [8]byte{0x40, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00},
	}))
	header, _, _ := client.readReply()
	require.Equal(t, RetSubmit, header.Command)

	engine.add([]byte{0x01, 0x60, 'c'})
	require.Equal(t, []byte{0x01, 0x60, 'c'}, client.receive(64).Buffer)
}

func TestReadAheadPurgeOnlyDropsThatPortsData(t *testing.T) {
	require.Equal(t, uint8(0x81), ftdiPortINEndpoint(0))
	require.Equal(t, uint8(0x81), ftdiPortINEndpoint(1))
	require.Equal(t, uint8(0x83), ftdiPortINEndpoint(2))

	engine := newScriptedINEngine([]byte{0x01, 0x60, 'a'}, []byte{0x01, 0x60, 'b'})
	client := startReadAheadSession(t, engine)
	require.Equal(t, []byte{0x01, 0x60, 'a'}, client.receive(64).Buffer)
	require.Eventually(t, func() bool { return engine.readCount() == 2 }, time.Second, 5*time.Millisecond)

	// Purge RX on port B (wIndex 2) while port A (endpoint 0x81) has data queued.
	client.seq++
	require.NoError(t, WriteSubmitCommand(client.conn, SubmitCommand{
		Header:          DataHeader{Command: CmdSubmit, SeqNum: client.seq, DevID: 7, Direction: USBIPDirOut, Endpoint: 0},
		NumberOfPackets: nonIsoPacketCount,
		Setup:           [8]byte{0x40, 0x00, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00},
	}))
	header, _, _ := client.readReply()
	require.Equal(t, RetSubmit, header.Command)
	require.Equal(t, []byte{0x01, 0x60, 'b'}, client.receive(64).Buffer)
}

func TestReadAheadPolicyOnlyForFullSpeedFTDI(t *testing.T) {
	require.NotNil(t, serialReadAheadPolicy(ftdiVendorID, SpeedFull))
	require.Nil(t, serialReadAheadPolicy(ftdiVendorID, SpeedHigh))
	require.Nil(t, serialReadAheadPolicy(0x1b20, SpeedFull))
}
