//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	benchClient = &net.TCPAddr{IP: net.IPv4(192, 168, 31, 10), Port: 51000}
	otherClient = &net.TCPAddr{IP: net.IPv4(192, 168, 31, 11), Port: 51000}
)

func startDenyTestServer(t *testing.T) (*ServerService, string) {
	t.Helper()
	export := &releaseTestExport{busid: "4-2", label: "adb"}
	host := &releaseTestHost{exports: map[string]Export{"4-2": export}}
	denyPath := filepath.Join(t.TempDir(), "deny.json")
	return startTestServer(t, host, ServerOptions{Devices: []DeviceMatch{{BusID: "4-2"}}, DenyPath: denyPath}), denyPath
}

func writeDenyFile(t *testing.T, path string, entries ...denyEntry) {
	t.Helper()
	raw, err := json.Marshal(entries)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o644))
	// The server notices a change by modification time and size.
	later := time.Now().Add(time.Duration(len(entries)) * time.Second)
	require.NoError(t, os.Chtimes(path, later, later))
}

// requireImportRejected asks for busid as a client at remote and expects an error reply.
func requireImportRejected(t *testing.T, server *ServerService, busid string, remote net.Addr) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	accepted := make(chan bool, 1)
	go func() { accepted <- server.handleImportBusID(remoteConn{Conn: serverConn, remote: remote}, busid) }()
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	header, err := ReadOpHeader(clientConn)
	require.NoError(t, err)
	require.Equal(t, OpStatusError, header.Status)
	require.False(t, <-accepted)
}

// controlOverPipeFrom opens a control session as a client at remote, sending hello (nil: an old client).
func controlOverPipeFrom(t *testing.T, server *ServerService, remote net.Addr, hello any) net.Conn {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	go server.handleControlConn(remoteConn{Conn: serverConn, remote: remote})
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))
	require.NoError(t, writeControlMessage(clientConn, controlFrame{Type: controlFrameHello, Version: controlProtocolVersion}, hello))
	var reader controlReader
	ack, err := reader.read(clientConn)
	require.NoError(t, err)
	require.Equal(t, controlFrameAck, ack.Frame.Type)
	require.Empty(t, ack.Payload, "older clients reject an ack with a payload")
	return clientConn
}

func readControlFrame(t *testing.T, conn net.Conn, timeout time.Duration) (controlMessage, error) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var reader controlReader
	message, err := reader.read(conn)
	if err == nil {
		message.Payload = append([]byte(nil), message.Payload...)
	}
	return message, err
}

// requireNotice skips snapshots until a notice arrives on conn.
func requireNotice(t *testing.T, conn net.Conn) ControlNotice {
	t.Helper()
	for {
		message, err := readControlFrame(t, conn, 2*time.Second)
		require.NoError(t, err)
		if message.Frame.Type == controlFrameNotice {
			var notice ControlNotice
			require.NoError(t, json.Unmarshal(message.Payload, &notice))
			return notice
		}
		require.Equal(t, controlFrameDeviceSnapshot, message.Frame.Type)
	}
}

// requireNoNotice reads frames for a while and fails on a notice.
func requireNoNotice(t *testing.T, conn net.Conn) {
	t.Helper()
	for {
		message, err := readControlFrame(t, conn, 200*time.Millisecond)
		if err != nil {
			var netErr net.Error
			require.ErrorAs(t, err, &netErr)
			require.True(t, netErr.Timeout())
			return
		}
		require.Equal(t, controlFrameDeviceSnapshot, message.Frame.Type)
	}
}

func TestServerBlocksAKickedClient(t *testing.T) {
	server, denyPath := startDenyTestServer(t)
	conn := importOverPipeFrom(t, server, "4-2", benchClient)

	until := time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
	writeDenyFile(t, denyPath, denyEntry{Addr: "192.168.31.10", BusIDs: []string{"4-2"}, Until: until, Reason: "lunch"})
	server.reloadDenyList()

	// Kicking closes the client's session; its retries are refused while another client gets the device.
	requireClosedByServer(t, conn)
	require.Eventually(t, func() bool {
		devices := server.DeviceSnapshot()
		return len(devices) == 1 && devices[0].State == DeviceStateIdle
	}, 2*time.Second, 10*time.Millisecond)
	requireImportRejected(t, server, "4-2", benchClient)
	other := importOverPipeFrom(t, server, "4-2", otherClient)
	require.NoError(t, other.Close())

	// Unblocking lets the client import again.
	require.Eventually(t, func() bool {
		return len(server.DeviceSnapshot()) == 1 && server.DeviceSnapshot()[0].State == DeviceStateIdle
	}, 2*time.Second, 10*time.Millisecond)
	writeDenyFile(t, denyPath)
	server.reloadDenyList()
	importOverPipeFrom(t, server, "4-2", benchClient)
}

func TestServerNoticesOnlyClientsThatAskForThem(t *testing.T) {
	server, denyPath := startDenyTestServer(t)
	newClient := controlOverPipeFrom(t, server, benchClient, controlHello{ClientID: "mac-1", Host: "mac", User: "dev", Caps: []string{controlCapNotice}})
	oldClient := controlOverPipeFrom(t, server, benchClient, nil)
	elsewhere := controlOverPipeFrom(t, server, otherClient, controlHello{Caps: []string{controlCapNotice}})

	writeDenyFile(t, denyPath, denyEntry{Addr: "192.168.31.10", Reason: "lunch"})
	server.reloadDenyList()

	notice := requireNotice(t, newClient)
	require.Empty(t, notice.BusIDs, "the block covers every device")
	require.Nil(t, notice.Until, "the block lasts until the owner lifts it")
	require.Equal(t, "lunch", notice.Reason)
	requireNoNotice(t, oldClient)
	requireNoNotice(t, elsewhere)

	// A client that connects while blocked is told after its first snapshot.
	late := controlOverPipeFrom(t, server, benchClient, controlHello{Caps: []string{controlCapNotice}})
	message, err := readControlFrame(t, late, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, controlFrameDeviceSnapshot, message.Frame.Type)
	require.Equal(t, "lunch", requireNotice(t, late).Reason)
}

func TestServerNamesTheHolderFromItsControlHello(t *testing.T) {
	server, _ := startDenyTestServer(t)
	importOverPipeFrom(t, server, "4-2", benchClient)
	// One Mac opens a control connection per device: one client id, one identity.
	for range 2 {
		controlOverPipeFrom(t, server, benchClient, controlHello{ClientID: "mac-1", Host: "mac", User: "dev", Caps: []string{controlCapNotice}})
	}
	holder := server.DeviceSnapshot()[0].Holder
	require.NotNil(t, holder)
	require.Equal(t, ControlDeviceHolder{Addr: "192.168.31.10", Since: holder.Since, Host: "mac", User: "dev"}, *holder)

	// A second client behind the same address leaves only the address.
	controlOverPipeFrom(t, server, benchClient, controlHello{ClientID: "mac-2", Host: "other", Caps: []string{controlCapNotice}})
	holder = server.DeviceSnapshot()[0].Holder
	require.Equal(t, ControlDeviceHolder{Addr: "192.168.31.10", Since: holder.Since, Multiple: true}, *holder)
}
