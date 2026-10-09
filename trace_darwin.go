//go:build darwin && !ios && cgo

package usbip

import (
	"fmt"
	"os"
	"time"
)

// SING_USBIP_DARWIN_TRACE=1 writes every IOUSBHostControllerInterface command,
// doorbell, transfer and completion to stderr, with transfer structure
// addresses and endpoint state machine states, for diagnosing
// IOUSBHostCIExceptionType faults.
var darwinTraceEnabled = os.Getenv("SING_USBIP_DARWIN_TRACE") != ""

func darwinTrace(format string, args ...any) {
	if !darwinTraceEnabled {
		return
	}
	fmt.Fprintf(os.Stderr, "%s usbip-darwin %s\n", time.Now().Format("15:04:05.000000"), fmt.Sprintf(format, args...))
}

const darwinEndpointStateHalted = 1

// darwinEndpointLabel names an endpoint by the USB/IP devid of its virtual
// controller, so traces of several imported devices stay distinguishable.
func darwinEndpointLabel(devID uint32, device uint8, endpoint uint8) string {
	return fmt.Sprintf("%x/%d.%02x", devID, device, endpoint)
}

func darwinEndpointStateName(state uint8) string {
	switch state {
	case 0:
		return "destroyed"
	case 1:
		return "halted"
	case 2:
		return "paused"
	case 3:
		return "active"
	default:
		return fmt.Sprintf("state(%d)", state)
	}
}
