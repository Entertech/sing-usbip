//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

type labelTestExport struct {
	entry DeviceEntry
}

func (e *labelTestExport) BusID() string { return e.entry.Info.BusIDString() }

func (e *labelTestExport) Snapshot(busy bool) ExportSnapshot {
	state := DeviceStateIdle
	if busy {
		state = DeviceStateAttached
	}
	return ExportSnapshot{Entry: e.entry, Backend: BackendIDLinuxSysfs, StableID: "stable-" + e.BusID(), State: state}
}

func (e *labelTestExport) DeviceInfo() (DeviceInfoTruncated, error) { return e.entry.Info, nil }

func (e *labelTestExport) NewServerDataSession(context.Context, net.Conn) (DataSession, error) {
	return nil, io.EOF
}

func TestMatchesName(t *testing.T) {
	t.Parallel()
	key := DeviceKey{BusID: "3-3", VendorID: 0x1d6b, ProductID: 0x0102, Name: "isp"}
	require.True(t, matches(DeviceMatch{Name: "isp"}, key))
	require.True(t, matches(DeviceMatch{BusID: "3-3", Name: "isp"}, key))
	require.False(t, matches(DeviceMatch{Name: "uart"}, key))
	require.False(t, matches(DeviceMatch{BusID: "3-3", Name: "isp"}, DeviceKey{BusID: "3-3"}))

	stripped := withoutNames([]DeviceMatch{{BusID: "3-3", Name: "isp"}})
	require.True(t, matches(stripped[0], DeviceKey{BusID: "3-3"}))
	require.False(t, DeviceMatch{Name: "isp"}.selectsDevice())
	require.True(t, DeviceMatch{BusID: "3-3", Name: "isp"}.selectsDevice())
}

func TestLedgerPublishesLabelsAndServerName(t *testing.T) {
	t.Parallel()
	labelled := testControlDeviceEntry()
	unlabelled := testControlDeviceEntry()
	copy(unlabelled.Info.BusID[:], "1-3")
	ledger := newExportLedger(nil, "photo", []DeviceMatch{
		{BusID: "1-2", VendorID: 0x046d, Name: "receiver"},
		{BusID: "1-3"},
	})
	ledger.ApplyHostSnapshot(map[string]Export{
		"1-2": &labelTestExport{entry: labelled},
		"1-3": &labelTestExport{entry: unlabelled},
	}, nil)
	ledger.SeedBroadcastState()

	labels := make(map[string]string)
	for _, device := range ledger.StateSnapshot() {
		labels[device.BusID] = device.Label
	}
	require.Equal(t, map[string]string{"1-2": "receiver", "1-3": ""}, labels)

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	sub := ledger.Subscribe(serverConn)
	defer ledger.Unsubscribe(sub)
	go func() {
		preface := make([]byte, len(controlPreface))
		if _, err := io.ReadFull(serverConn, preface); err != nil {
			return
		}
		var reader controlReader
		if _, err := reader.read(serverConn); err != nil {
			return
		}
		if err := writeControlMessage(serverConn, controlFrame{Type: controlFrameAck, Version: controlProtocolVersion}, nil); err != nil {
			return
		}
		message := <-sub.send
		_ = writeControlMessage(serverConn, message.Frame, message.Payload)
	}()
	server, devices, err := FetchControlSnapshot(clientConn)
	require.NoError(t, err)
	require.Equal(t, "photo", server)
	require.Len(t, devices, 2)

	entries := controlDeviceInfoToEntries(devices, true)
	var names []string
	for _, entry := range entries {
		names = append(names, entryDeviceKey(entry).Name)
	}
	require.ElementsMatch(t, []string{"receiver", ""}, names)
}
