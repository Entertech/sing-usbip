# sing-usbip

Cross-platform USB/IP implementation in Go

## Entertech Windows control transfer fix

Windows exports originally submitted every control URB to VBoxUSB with
`DirectionSetup`. VBoxUSB treats any direction other than `DirectionIn` as an
OUT transfer, so an IN setup packet such as GET_DESCRIPTOR was submitted with
the wrong transfer direction. On a Windows export of an FT232R (0403:6001) and
a composite USB gadget (1d6b:0102), descriptor reads returned EPIPE/STALL before
macOS could enumerate the device.

`controlSubmit` now preserves the USB/IP request direction, as bulk transfers
already do. With that change, real Windows exports returned the correct 8-byte
and 18-byte device descriptors for those devices and a HID device (2e3c:af01).
The existing tests also passed on Windows and macOS; privileged macOS HCI tests
were not included in that unit-test result. This confirms the Windows control
transfer fix, not complete macOS import compatibility: macOS 26.5.1 still
reported `IOUSBHostCIExceptionTypeProtocolError` during device configuration in
the subsequent native import experiment. That separate issue remains open.

See [VBoxUSB control transfer direction handling](https://github.com/mirror/vbox/blob/master/src/VBox/HostDrivers/VBoxUSB/win/dev/VBoxUsbRt.cpp)
and [usbipd-win control URB construction](https://github.com/dorssel/usbipd-win/blob/master/Usbipd/AttachedEndpoint.cs).

Backends:

| Platform | Server (export)                  | Client (import)                       |
|----------|----------------------------------|---------------------------------------|
| Linux    | `usbip-host` kernel driver       | `vhci_hcd` kernel driver              |
| macOS    | IOUSBHost (disable SIP required) | IOUSBHostControllerInterface          |
| Windows  | VBoxUSB drivers (bundled)        | usbip-win2 UDE vhci driver (bundled)  |

The Windows drivers are embedded in the binary by default. Builds with the
`with_external_usbip_drivers` tag instead load them from the executable
directory, verified against the digests in `driverassets`; regenerate the
digests with `go generate ./driverassets` after replacing driver files.

## Control extension

Standard USB/IP has no change notifications, so importers must poll for
device arrivals and removals. sing-usbip adds a control channel that pushes
device state to the client instead. It runs on its own connections alongside
the standard protocol, so the server keeps serving standard usbip clients
unchanged; the sing-usbip client always imports through the extension and
requires a sing-usbip server.

## CLI

Export devices (matched by busid, vendor:product, and/or serial):

```
sing-usbip list
sing-usbip run -s :3240 -d 1-1.2
sing-usbip run -s 127.0.0.1:3240 -d 0bda:8153,serial=001000001
```

Import devices (without `--device`, every exported device is imported):

```
sing-usbip list -c 192.168.1.10
sing-usbip run -c 192.168.1.10 -d 0bda:8153
```

Exported devices can also be attached by standard usbip clients; importing
requires a sing-usbip server.
