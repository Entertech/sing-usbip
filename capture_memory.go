//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
)

// captureMemory remembers the identities (VID:PID) seen on busids whose rule
// pins only the port, so a server can capture such a device the moment it
// enumerates instead of after the operating system's own driver has bound it.
// A device that switches identity on one port (a board dropping from its
// application to its bootloader) otherwise reaches the host's class driver
// first: on Windows a mass-storage bootloader is mounted by USBSTOR, which can
// leave the device mid-command before it is taken away for export.
//
// With a path the identities persist across restarts as JSON
// ({"4-2": ["1d6b:1001", "1b20:0301"]}); without one they last for the process.
type captureMemory struct {
	path string

	access sync.Mutex
	seen   map[string][]deviceIdentity
}

type deviceIdentity struct {
	VendorID  uint16
	ProductID uint16
}

func (i deviceIdentity) String() string {
	return fmt.Sprintf("%04x:%04x", i.VendorID, i.ProductID)
}

func parseDeviceIdentity(text string) (deviceIdentity, bool) {
	var identity deviceIdentity
	n, err := fmt.Sscanf(text, "%04x:%04x", &identity.VendorID, &identity.ProductID)
	return identity, err == nil && n == 2 && len(text) == 9
}

func newCaptureMemory(path string) *captureMemory {
	return &captureMemory{path: path, seen: make(map[string][]deviceIdentity)}
}

// load reads the file; a missing file is an empty memory.
func (m *captureMemory) load() error {
	if m.path == "" {
		return nil
	}
	data, err := os.ReadFile(m.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return E.Cause(err, "read capture memory ", m.path)
	}
	var stored map[string][]string
	err = json.Unmarshal(data, &stored)
	if err != nil {
		return E.Cause(err, "parse capture memory ", m.path)
	}
	seen := make(map[string][]deviceIdentity, len(stored))
	for busid, identities := range stored {
		for _, text := range identities {
			identity, ok := parseDeviceIdentity(strings.ToLower(text))
			if !ok {
				return E.New("capture memory ", m.path, ": invalid identity ", text, " for ", busid)
			}
			if !slices.Contains(seen[busid], identity) {
				seen[busid] = append(seen[busid], identity)
			}
		}
	}
	m.access.Lock()
	m.seen = seen
	m.access.Unlock()
	return nil
}

// remember records identity on busid and reports whether it was new; a new
// identity is written out before remember returns.
func (m *captureMemory) remember(busid string, identity deviceIdentity) (bool, error) {
	m.access.Lock()
	defer m.access.Unlock()
	if slices.Contains(m.seen[busid], identity) {
		return false, nil
	}
	m.seen[busid] = append(m.seen[busid], identity)
	return true, m.saveLocked()
}

// identities returns busid's remembered identities.
func (m *captureMemory) identities(busid string) []deviceIdentity {
	m.access.Lock()
	defer m.access.Unlock()
	return slices.Clone(m.seen[busid])
}

// busIDs returns every busid with a remembered identity, sorted.
func (m *captureMemory) busIDs() []string {
	m.access.Lock()
	defer m.access.Unlock()
	out := make([]string, 0, len(m.seen))
	for busid := range m.seen {
		out = append(out, busid)
	}
	slices.Sort(out)
	return out
}

func (m *captureMemory) saveLocked() error {
	if m.path == "" {
		return nil
	}
	stored := make(map[string][]string, len(m.seen))
	for busid, identities := range m.seen {
		for _, identity := range identities {
			stored[busid] = append(stored[busid], identity.String())
		}
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(m.path), filepath.Base(m.path)+".*.tmp")
	if err != nil {
		return E.Cause(err, "write capture memory ", m.path)
	}
	_, err = temporary.Write(append(data, '\n'))
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), m.path)
	}
	if err != nil {
		_ = os.Remove(temporary.Name())
		return E.Cause(err, "write capture memory ", m.path)
	}
	return nil
}

// portOnlyRule reports whether busid is exported by a rule that pins only the
// port, so whatever enumerates there is exported, under any identity.
func portOnlyRule(matches []DeviceMatch, busid string) bool {
	for _, match := range matches {
		if match.BusID == busid && match.VendorID == 0 && match.ProductID == 0 && match.Serial == "" {
			return true
		}
	}
	return false
}

// busIDPort returns the hub port of a Windows "hub-port" busid.
func busIDPort(busid string) (uint16, bool) {
	index := strings.LastIndexByte(busid, '-')
	if index < 0 {
		return 0, false
	}
	port, err := strconv.ParseUint(busid[index+1:], 10, 16)
	return uint16(port), err == nil
}
