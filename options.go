package usbip

import (
	"context"
	"net"

	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

type ListenFunc func(ctx context.Context) (net.Listener, error)

// DeviceMatch selects devices. Name is a label: a server attaches it to the
// exports a match selects and publishes it on the control channel, and a
// client can select devices by that label alone.
type DeviceMatch struct {
	BusID     string
	VendorID  uint16
	ProductID uint16
	Serial    string
	Name      string
}

func (m DeviceMatch) IsZero() bool {
	return m.BusID == "" && m.VendorID == 0 && m.ProductID == 0 && m.Serial == "" && m.Name == ""
}

// selectsDevice reports whether the match names a local device by its
// attributes, not only by label.
func (m DeviceMatch) selectsDevice() bool {
	return m.BusID != "" || m.VendorID != 0 || m.ProductID != 0 || m.Serial != ""
}

type ServerOptions struct {
	Logger logger.ContextLogger
	// Name identifies the server to clients on the control channel.
	Name          string
	Devices       []DeviceMatch
	Listen        ListenFunc
	ListenAddress M.Socksaddr
	// CaptureMemoryPath (Windows) is the file that keeps the identities seen on
	// busids exported by a port-only rule, so their devices are captured as they
	// enumerate (see captureMemory). Empty keeps them for the process only.
	CaptureMemoryPath string
}

// exportHostOptions are the ServerOptions an export host uses besides the
// device matches.
type exportHostOptions struct {
	captureMemoryPath string
}

type ClientOptions struct {
	Logger        logger.ContextLogger
	Dialer        N.Dialer
	ServerAddress M.Socksaddr
	Devices       []DeviceMatch
}
