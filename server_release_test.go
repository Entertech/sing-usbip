//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type releaseTestExport struct {
	busid string
	label string
}

func (e *releaseTestExport) BusID() string { return e.busid }

func (e *releaseTestExport) Snapshot(busy bool) ExportSnapshot {
	state := DeviceStateIdle
	if busy {
		state = DeviceStateAttached
	}
	var entry DeviceEntry
	copy(entry.Info.BusID[:], e.busid)
	entry.Product = e.label
	return ExportSnapshot{Entry: entry, StableID: e.label, State: state}
}

func (e *releaseTestExport) DeviceInfo() (DeviceInfoTruncated, error) {
	var info DeviceInfoTruncated
	copy(info.BusID[:], e.busid)
	return info, nil
}

// idleEngine is a device with no traffic: the session only ends when its
// connection does.
type idleEngine struct{}

func (idleEngine) Submit(URBRequest) URBResponse { return URBResponse{} }
func (idleEngine) AbortEndpoint(uint8) error     { return nil }
func (idleEngine) Close() error                  { return nil }

func (e *releaseTestExport) NewServerDataSession(ctx context.Context, conn net.Conn) (DataSession, error) {
	return newUserspaceURBSession(ctx, nopLogger(), conn, idleEngine{}), nil
}

type releaseTestHost struct {
	access   sync.Mutex
	exports  map[string]Export
	released []string
	finished []string
}

func (h *releaseTestHost) Start() error { return nil }
func (h *releaseTestHost) Close() error { return nil }

func (h *releaseTestHost) Events() (<-chan struct{}, error) { return make(chan struct{}), nil }

func (h *releaseTestHost) Reconcile(func(string) bool) (map[string]Export, []string, error) {
	h.access.Lock()
	defer h.access.Unlock()
	snapshot := make(map[string]Export, len(h.exports))
	for busid, export := range h.exports {
		snapshot[busid] = export
	}
	released := h.released
	h.released = nil
	return snapshot, released, nil
}

func (h *releaseTestHost) FinishImport(busid string) (bool, error) {
	h.access.Lock()
	defer h.access.Unlock()
	h.finished = append(h.finished, busid)
	return false, nil
}

func (h *releaseTestHost) finishedBusIDs() []string {
	h.access.Lock()
	defer h.access.Unlock()
	return append([]string(nil), h.finished...)
}

func startReleaseTestServer(t *testing.T, host *releaseTestHost) *ServerService {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	server := newServerServiceWithHost(ctx, cancel, nopLogger(), ServerOptions{Devices: []DeviceMatch{{BusID: "4-2"}}}, host)
	require.NoError(t, server.reconcileAndBroadcast(false))
	t.Cleanup(func() { _ = server.Close() })
	return server
}

// importOverPipe imports busid and returns the client end of the data connection.
func importOverPipe(t *testing.T, server *ServerService, busid string) net.Conn {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	accepted := make(chan bool, 1)
	go func() { accepted <- server.handleImportBusID(serverConn, busid) }()
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	header, err := ReadOpHeader(clientConn)
	require.NoError(t, err)
	require.Equal(t, OpStatusOK, header.Status)
	_, err = ReadOpRepImportBody(clientConn)
	require.NoError(t, err)
	require.True(t, <-accepted)
	_ = clientConn.SetReadDeadline(time.Time{})
	return clientConn
}

func requireClosedByServer(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "the server should close the session's connection")
}

func TestServerClosesImportWhenItsExportIsReleased(t *testing.T) {
	ufu := &releaseTestExport{busid: "4-2", label: "ufu"}
	host := &releaseTestHost{exports: map[string]Export{"4-2": ufu}}
	server := startReleaseTestServer(t, host)
	conn := importOverPipe(t, server, "4-2")
	require.True(t, server.ledger.IsReserved("4-2"))

	// The device comes back on the same port with another identity while the
	// session is idle (no URB in flight that could fail).
	adb := &releaseTestExport{busid: "4-2", label: "adb"}
	host.access.Lock()
	host.exports["4-2"] = adb
	host.released = []string{"4-2"}
	host.access.Unlock()
	require.NoError(t, server.reconcileAndBroadcast(true))

	requireClosedByServer(t, conn)
	require.Eventually(t, func() bool {
		server.sessionsAccess.Lock()
		defer server.sessionsAccess.Unlock()
		return len(server.sessions) == 0
	}, 2*time.Second, 10*time.Millisecond)
	require.Empty(t, host.finishedBusIDs(), "the released session must not finish the new export's import")
	require.True(t, server.ledger.HasExport("4-2", adb))
	require.False(t, server.ledger.IsReserved("4-2"))

	// The new export can be imported.
	importOverPipe(t, server, "4-2")
	require.True(t, server.ledger.IsReserved("4-2"))
}

func TestServerFinishesImportWhenTheClientLeaves(t *testing.T) {
	export := &releaseTestExport{busid: "4-2", label: "adb"}
	host := &releaseTestHost{exports: map[string]Export{"4-2": export}}
	server := startReleaseTestServer(t, host)
	conn := importOverPipe(t, server, "4-2")

	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool { return len(host.finishedBusIDs()) == 1 }, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"4-2"}, host.finishedBusIDs())
	require.False(t, server.ledger.IsReserved("4-2"))
}
