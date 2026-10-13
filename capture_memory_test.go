//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCaptureMemoryPersistsIdentitiesPerBusID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture-memory.json")
	memory := newCaptureMemory(path)
	require.NoError(t, memory.load(), "a missing file is an empty memory")

	adb := deviceIdentity{VendorID: 0x1d6b, ProductID: 0x1001}
	ufu := deviceIdentity{VendorID: 0x1b20, ProductID: 0x0301}
	added, err := memory.remember("4-2", adb)
	require.NoError(t, err)
	require.True(t, added)
	added, err = memory.remember("4-2", adb)
	require.NoError(t, err)
	require.False(t, added)
	_, err = memory.remember("4-2", ufu)
	require.NoError(t, err)

	reloaded := newCaptureMemory(path)
	require.NoError(t, reloaded.load())
	require.Equal(t, []string{"4-2"}, reloaded.busIDs())
	require.Equal(t, []deviceIdentity{adb, ufu}, reloaded.identities("4-2"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), `"1b20:0301"`)
}

func TestCaptureMemoryRejectsAMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture-memory.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"4-2": ["1b20-0301"]}`), 0o644))
	require.Error(t, newCaptureMemory(path).load())
}

func TestCaptureMemoryWithoutAPathStaysInMemory(t *testing.T) {
	memory := newCaptureMemory("")
	added, err := memory.remember("4-2", deviceIdentity{VendorID: 0x1b20, ProductID: 0x0301})
	require.NoError(t, err)
	require.True(t, added)
	require.Len(t, memory.identities("4-2"), 1)
}

func TestPortOnlyRule(t *testing.T) {
	matches := []DeviceMatch{
		{BusID: "4-2", Name: "isp"},
		{BusID: "4-1", VendorID: 0x0403, ProductID: 0x6001, Name: "uart"},
	}
	require.True(t, portOnlyRule(matches, "4-2"))
	require.False(t, portOnlyRule(matches, "4-1"), "a rule that pins the identity captures only that device")
	require.False(t, portOnlyRule(matches, "4-3"))

	port, ok := busIDPort("4-2")
	require.True(t, ok)
	require.Equal(t, uint16(2), port)
	_, ok = busIDPort("4")
	require.False(t, ok)
}
