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
were not included in that unit-test result. The macOS import failure that
followed is covered in the next section.

See [VBoxUSB control transfer direction handling](https://github.com/mirror/vbox/blob/master/src/VBox/HostDrivers/VBoxUSB/win/dev/VBoxUsbRt.cpp)
and [usbipd-win control URB construction](https://github.com/dorssel/usbipd-win/blob/master/Usbipd/AttachedEndpoint.cs).

## Entertech macOS endpoint pause fix

On macOS 26.5.1 (arm64), importing the devices above through
IOUSBHostControllerInterface failed every time with
`IOUSBHostCIExceptionTypeProtocolError`, and later
`IOUSBHostCIExceptionTypeCommandTimeout`, before any driver attached. The
client ran as root with SIP disabled.

Running with `SING_USBIP_DARWIN_TRACE=1` showed two causes, both in how
endpoint 0 handled `EndpointPause` arriving while a control request was in
flight. macOS sends these pauses repeatedly during enumeration.

- **SET_CONFIGURATION status stage.**
  - What happened: the request was UNLINKed and the status transfer completed
    with `EndpointStopped`. The endpoint state machine keeps the stopped
    transfer current, and the kernel resumed that same transfer about 100 ms
    later without `EndpointSetNextTransfer`. The setup packet had already been
    discarded, so the worker reported success without re-issuing the request,
    and the controller faulted with `ProtocolError`.
  - For comparison: in a test build, an ep0 data stage reported as
    `EndpointStopped` was resumed the same way, also without
    `EndpointSetNextTransfer`, and re-executing it was accepted. Bulk
    endpoints get `EndpointSetNextTransfer` before resuming.
- **GET_DESCRIPTOR(BOS) STALL during the pause.**
  - What happened: once ep0 waited for its in-flight request, a USB 2.0 gadget
    could STALL GET_DESCRIPTOR(BOS) during that wait. The stall completion
    halts the endpoint, and the state machine then refuses a Success response
    to the pause. An unanswered command is fatal (`CommandTimeout`).

The fix, in `endpoint_darwin.go`:

- Commands are inspected before the in-flight transfer is retired, as the SDK
  documents.
- On ep0, a pause waits up to 500 ms for the request to finish and reports its
  real status. If that fails, the pause falls back to UNLINK/`EndpointStopped`.
- The setup packet is kept until the status stage completes, so a resumed
  stopped stage re-issues the request.
- If the endpoint halted while quiescing, the pause is answered with
  `NotPermitted`. The kernel then follows with `EndpointReset`,
  `EndpointSetNextTransfer` and a doorbell.

Regression on real Windows exports, run twice in a row with all four devices
imported at once:

- FT232R serial port appears and receives data. Closing and reopening it,
  which pauses and resumes bulk IN, keeps working.
- FT2232 exposes both ports.
- ISP gadget is listed by adb, and a 16 MiB `adb exec-out` read runs at
  7.5-8.9 MB/s.
- HID power supply attaches.

The unpatched build reproduced the `ProtocolError` in the same setup.

## Entertech device labels and server name

Bench tooling needed device names that live with the server, not in every
client's configuration.

- A device match takes `name=<label>`, for example
  `-d 3-3,1d6b:0102,name=isp`.
  - On a server, the label is attached to the exports the match selects and
    published in the control snapshot (`label`). The server ignores it when
    matching local devices.
  - On a client, `-d name=isp` imports by label alone.
- `run -s … --name <server>` names the server; it defaults to the host name.
  The name is sent in the snapshot as `server`.
- `list -c` prints the server name and a `LABEL` column. `--json` prints the
  same data for scripts.

Both fields are optional JSON members, so older clients ignore them.

## Entertech Windows export reports absent devices

When a Windows export's device left the bus while a client was importing it,
`Reconcile` skipped the reserved export and scheduled no recheck. The export
stayed listed as `idle` indefinitely, and every import waited 10 s for a
VBoxUSB interface that never appeared. This was seen with an ISP gadget that
powered down.

An export whose device is absent is now reported `unavailable` ("device not
present"), so clients stop importing it. A reserved absent export gets a
recheck, which releases it after the existing 10 s absence grace. On a real
Windows export, powering the gadget off showed `unavailable` within 2 s, and
the export was released after 10 s with no rejected imports. Powering it back
on re-captured the device, and the client re-attached about 12 s later.

## Entertech remote list shows attached devices

`sing-usbip list -c` used to hide devices that another client had attached,
so a busy device looked the same as one that was not exported. It now lists
every export with a `STATE` column (`idle`, `attached` or `unavailable`),
taken from the control channel snapshot. Importing is unchanged: the client
still only imports idle devices and waits for attached ones to be released.

Known open items:

- A RET_SUBMIT that arrives after CMD_UNLINK is still reported as canceled,
  and its IN data is dropped.
- Each endpoint keeps one URB in flight, which caps bulk throughput over
  network latency.
- Apple documents that the interface does not support isochronous endpoints
  or bulk streams.

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
