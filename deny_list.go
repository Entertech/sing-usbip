//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

const (
	denyListPollInterval = 2 * time.Second
	denyLogInterval      = time.Minute
)

// denyEntry blocks the client at Addr from the listed exports (every export
// when BusIDs is empty) until Until (until it is removed when empty). Imports
// carry no client identity, so the address is the only key a block can have.
type denyEntry struct {
	Addr   string   `json:"addr"`
	BusIDs []string `json:"busids,omitempty"`
	Until  string   `json:"until,omitempty"`
	Reason string   `json:"reason,omitempty"`

	until time.Time
}

func (e denyEntry) covers(addr, busid string, now time.Time) bool {
	if e.Addr != addr || !e.until.IsZero() && !now.Before(e.until) {
		return false
	}
	return len(e.BusIDs) == 0 || slices.Contains(e.BusIDs, busid)
}

func (e denyEntry) notice() ControlNotice {
	notice := ControlNotice{BusIDs: e.BusIDs, Reason: e.Reason}
	if !e.until.IsZero() {
		until := e.until
		notice.Until = &until
	}
	return notice
}

func (e denyEntry) key() string {
	return e.Addr + "|" + strings.Join(e.BusIDs, ",") + "|" + e.Until + "|" + e.Reason
}

// denyList is the bench owner's block file (control.ps1 kick/unblock writes
// it), read again whenever it changes.
type denyList struct {
	path string

	access   sync.Mutex
	modTime  time.Time
	size     int64
	present  bool
	entries  []denyEntry
	lastLogs map[string]time.Time
}

func newDenyList(path string) *denyList {
	return &denyList{path: path, lastLogs: make(map[string]time.Time)}
}

// reload reads the file if it changed since the last read and returns the
// entries that are new. A missing file is an empty list.
func (d *denyList) reload() (added []denyEntry, changed bool, err error) {
	info, statErr := os.Stat(d.path)
	present := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, false, statErr
	}
	d.access.Lock()
	defer d.access.Unlock()
	if present == d.present && (!present || info.ModTime().Equal(d.modTime) && info.Size() == d.size) {
		return nil, false, nil
	}
	var entries []denyEntry
	if present {
		entries, err = readDenyFile(d.path)
		if err != nil {
			return nil, false, err
		}
		d.modTime, d.size = info.ModTime(), info.Size()
	}
	d.present = present
	known := make(map[string]bool, len(d.entries))
	for _, entry := range d.entries {
		known[entry.key()] = true
	}
	for _, entry := range entries {
		if !known[entry.key()] {
			added = append(added, entry)
		}
	}
	d.entries = entries
	return added, true, nil
}

func readDenyFile(path string) ([]denyEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	if len(raw) == 0 {
		return nil, nil
	}
	// PowerShell 5.1 writes a one-element array as a bare object.
	if raw[0] == '{' {
		raw = append(append([]byte{'['}, raw...), ']')
	}
	var entries []denyEntry
	err = json.Unmarshal(raw, &entries)
	if err != nil {
		return nil, E.Cause(err, "parse ", path)
	}
	out := entries[:0]
	for _, entry := range entries {
		ip := net.ParseIP(strings.TrimSpace(entry.Addr))
		if ip == nil {
			return nil, E.New(path, ": invalid addr ", entry.Addr)
		}
		entry.Addr = ip.String()
		if entry.Until != "" {
			entry.until, err = time.Parse(time.RFC3339, entry.Until)
			if err != nil {
				return nil, E.Cause(err, path, ": until of ", entry.Addr)
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// blocking returns the entry that blocks addr from busid, if any.
func (d *denyList) blocking(addr, busid string, now time.Time) (denyEntry, bool) {
	d.access.Lock()
	defer d.access.Unlock()
	for _, entry := range d.entries {
		if entry.covers(addr, busid, now) {
			return entry, true
		}
	}
	return denyEntry{}, false
}

// active returns the unexpired entries for addr.
func (d *denyList) active(addr string, now time.Time) []denyEntry {
	d.access.Lock()
	defer d.access.Unlock()
	var out []denyEntry
	for _, entry := range d.entries {
		if entry.Addr == addr && (entry.until.IsZero() || now.Before(entry.until)) {
			out = append(out, entry)
		}
	}
	return out
}

// shouldLog limits rejection logs to one a minute per client and export: an
// older client retries a blocked import every five seconds.
func (d *denyList) shouldLog(addr, busid string, now time.Time) bool {
	d.access.Lock()
	defer d.access.Unlock()
	key := addr + "|" + busid
	if last, ok := d.lastLogs[key]; ok && now.Sub(last) < denyLogInterval {
		return false
	}
	d.lastLogs[key] = now
	return true
}
