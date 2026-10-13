//go:build linux || (darwin && cgo) || windows

package usbip

import (
	"fmt"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

// Serial read-ahead keeps a read posted on a full-speed serial adapter's bulk
// IN endpoint on the server, independent of the client's URBs.
//
// Without it the device is only read when a client URB arrives, one network
// round trip after the previous one completed. An FT232R holds 256 received
// bytes, which last 1.3 ms at 2 Mbaud, so any round trip longer than that loses
// bytes during a burst (2-80 ms over Wi-Fi in practice).
//
// The reader queues each completed transfer whole and hands it to the next
// client URB, so transfer boundaries (and the short packet that ends each one)
// are preserved. It only runs for adapters that never send unsolicited data a
// host must not see early: reading ahead on mass storage or a command/response
// protocol would consume data before the host asked for it.

const (
	readAheadMaxQueuedBytes = 64 << 10
	readAheadStopTimeout    = 3 * time.Second
	usbipStatusEOVERFLOW    = -75
	ftdiVendorID            = 0x0403
	ftdiStatusLength        = 2
	fullSpeedMaxPacket      = 64
)

type readAheadPolicy struct {
	// ftdi collapses FTDI's periodic status-only transfers (2 bytes, sent every
	// latency-timer tick even when idle) so an idle port does not grow the queue.
	ftdi bool
}

// serialReadAheadPolicy returns the policy for a device, or nil when read-ahead
// does not apply to it.
func serialReadAheadPolicy(vendorID uint16, speed uint32) *readAheadPolicy {
	if vendorID != ftdiVendorID || speed != SpeedFull {
		return nil
	}
	return &readAheadPolicy{ftdi: true}
}

type readAheadResult struct {
	status int32
	data   []byte
}

type readAheadEndpoint struct {
	endpoint uint8

	// template is the latest client URB on the endpoint; the reader copies its
	// length and transfer flags.
	template SubmitCommand

	queue       []readAheadResult
	queueBytes  int
	statusQueue bool // an error status is queued; the reader waits for it to be taken
	running     bool
	stopping    bool
	readerDone  chan struct{}
	wake        chan struct{}
	cancelled   bool // the waiting client URB was unlinked
}

type sessionReadAhead struct {
	session *userspaceURBSession
	policy  readAheadPolicy

	access    sync.Mutex
	endpoints map[uint8]*readAheadEndpoint
	closed    bool
}

func newSessionReadAhead(session *userspaceURBSession, policy readAheadPolicy) *sessionReadAhead {
	return &sessionReadAhead{
		session:   session,
		policy:    policy,
		endpoints: make(map[uint8]*readAheadEndpoint),
	}
}

// handles reports whether a submit goes through read-ahead: non-isochronous,
// non-interrupt bulk IN on an endpoint other than 0.
func (r *sessionReadAhead) handles(command SubmitCommand) bool {
	return r != nil &&
		command.Header.Endpoint != 0 &&
		command.Header.Direction == USBIPDirIn &&
		command.NumberOfPackets <= 0 &&
		command.Interval == 0 &&
		command.TransferBufferLength > 0
}

// owns reports whether endpoint has a read-ahead reader, so unlinking a
// submit there must not abort the device endpoint.
func (r *sessionReadAhead) owns(endpoint uint8) bool {
	if r == nil {
		return false
	}
	r.access.Lock()
	defer r.access.Unlock()
	_, found := r.endpoints[endpoint]
	return found
}

// take answers a client IN submit from the queue, waiting for the reader when
// it is empty. A submit unlinked while it waits gets ECONNRESET, which the
// session does not send back.
func (r *sessionReadAhead) take(command SubmitCommand) SubmitResponse {
	endpoint := commandEndpoint(command)
	response := SubmitResponse{
		Header: DataHeader{
			Command:   RetSubmit,
			SeqNum:    command.Header.SeqNum,
			DevID:     command.Header.DevID,
			Direction: command.Header.Direction,
			Endpoint:  command.Header.Endpoint,
		},
		StartFrame:      command.StartFrame,
		NumberOfPackets: command.NumberOfPackets,
	}
	r.access.Lock()
	state := r.endpoints[endpoint]
	if state == nil {
		state = &readAheadEndpoint{endpoint: endpoint, wake: make(chan struct{}, 1)}
		r.endpoints[endpoint] = state
	}
	state.template = command
	state.cancelled = false
	for {
		if r.closed {
			r.access.Unlock()
			response.Status = usbipStatusECONNRESET
			return response
		}
		if len(state.queue) > 0 {
			result := r.popLocked(state, int(command.TransferBufferLength))
			r.startLocked(state)
			r.access.Unlock()
			response.Status = result.status
			response.ActualLength = int32(len(result.data))
			if len(result.data) > 0 {
				response.Buffer = result.data
			}
			return response
		}
		if state.cancelled {
			state.cancelled = false
			r.access.Unlock()
			response.Status = usbipStatusECONNRESET
			return response
		}
		r.startLocked(state)
		r.access.Unlock()
		<-state.wake
		r.access.Lock()
	}
}

// popLocked removes the head transfer, splitting it at a packet boundary when
// the client's buffer is smaller than what was read: the first part is then a
// transfer that ended because the buffer filled, the rest the next transfer.
func (r *sessionReadAhead) popLocked(state *readAheadEndpoint, capacity int) readAheadResult {
	head := state.queue[0]
	if len(head.data) <= capacity {
		state.queue = state.queue[1:]
		state.queueBytes -= len(head.data)
		if head.status != 0 {
			state.statusQueue = false
		}
		return head
	}
	if capacity%fullSpeedMaxPacket != 0 {
		// No packet boundary to cut at: report the babble a real controller would.
		state.queue = state.queue[1:]
		state.queueBytes -= len(head.data)
		if head.status != 0 {
			state.statusQueue = false
		}
		return readAheadResult{status: usbipStatusEOVERFLOW, data: head.data[:capacity]}
	}
	state.queue[0].data = head.data[capacity:]
	state.queueBytes -= capacity
	return readAheadResult{data: head.data[:capacity]}
}

func (r *sessionReadAhead) startLocked(state *readAheadEndpoint) {
	if r.closed || state.running || state.stopping || state.statusQueue || state.queueBytes >= readAheadMaxQueuedBytes {
		return
	}
	state.running = true
	state.readerDone = make(chan struct{})
	go r.read(state, state.readerDone)
}

func (r *sessionReadAhead) read(state *readAheadEndpoint, done chan struct{}) {
	defer close(done)
	for {
		r.access.Lock()
		if r.closed || state.stopping || state.queueBytes >= readAheadMaxQueuedBytes {
			state.running = false
			r.access.Unlock()
			return
		}
		template := state.template
		r.access.Unlock()

		buffer := make([]byte, int(template.TransferBufferLength))
		result := r.session.engine.Submit(URBRequest{
			Command: SubmitCommand{
				Header: DataHeader{
					Command:   CmdSubmit,
					DevID:     template.Header.DevID,
					Direction: USBIPDirIn,
					Endpoint:  template.Header.Endpoint,
				},
				TransferFlags:        template.TransferFlags,
				TransferBufferLength: template.TransferBufferLength,
				NumberOfPackets:      nonIsoPacketCount,
			},
			Endpoint: state.endpoint,
			Buffer:   buffer,
		})

		r.access.Lock()
		if r.closed || state.stopping {
			// An abort ended this read; what it returned is not device data.
			state.running = false
			r.access.Unlock()
			return
		}
		if result.Error != nil {
			state.running = false
			r.access.Unlock()
			r.session.failFatal(E.Cause(result.Error, "read-ahead endpoint ", fmt.Sprintf("0x%02x", state.endpoint)))
			return
		}
		actual := min(max(int(result.ActualLength), 0), len(result.Buffer))
		r.enqueueLocked(state, readAheadResult{status: result.Status, data: result.Buffer[:actual]})
		stop := result.Status != 0
		if stop {
			// A halted endpoint keeps failing; read again once the client has
			// seen the error and resubmitted (after CLEAR_FEATURE if it cares).
			state.statusQueue = true
			state.running = false
		}
		select {
		case state.wake <- struct{}{}:
		default:
		}
		r.access.Unlock()
		if stop {
			return
		}
	}
}

func (r *sessionReadAhead) enqueueLocked(state *readAheadEndpoint, result readAheadResult) {
	if r.policy.ftdi && result.status == 0 && len(state.queue) > 0 {
		tail := &state.queue[len(state.queue)-1]
		if tail.status == 0 && len(tail.data) == ftdiStatusLength && len(result.data) >= ftdiStatusLength && ftdiSameStatus(tail.data, result.data) {
			// The queued status-only transfer says nothing the new one does
			// not: replace it instead of queueing another.
			state.queueBytes += len(result.data) - len(tail.data)
			*tail = result
			return
		}
	}
	state.queue = append(state.queue, result)
	state.queueBytes += len(result.data)
}

// ftdiSameStatus compares the modem status byte and the line status error bits
// (overrun, parity, framing, break, FIFO error) of two FTDI transfers; the
// data-ready and transmitter-empty bits change with traffic and carry no
// information a later transfer loses.
func ftdiSameStatus(a, b []byte) bool {
	return a[0] == b[0] && (a[1]^b[1])&0x9e == 0
}

// cancelWait wakes the client submit waiting on endpoint after the client
// unlinked it. The data the reader queues stays for the next submit.
func (r *sessionReadAhead) cancelWait(endpoint uint8) {
	r.access.Lock()
	defer r.access.Unlock()
	state := r.endpoints[endpoint]
	if state == nil {
		return
	}
	state.cancelled = true
	select {
	case state.wake <- struct{}{}:
	default:
	}
}

// beforeControl quiesces the readers a control request would race with or
// invalidate: SET_CONFIGURATION, SET_INTERFACE and CLEAR_FEATURE(ENDPOINT_HALT)
// reset the device side of the pipes, and an FTDI reset or RX purge discards
// what that port had received, which its queue stands in for.
func (r *sessionReadAhead) beforeControl(setup [8]byte) {
	if r == nil {
		return
	}
	bmRequestType, bRequest := setup[0], setup[1]
	wValue := uint16(setup[2]) | uint16(setup[3])<<8
	wIndex := uint16(setup[4]) | uint16(setup[5])<<8
	switch {
	case bmRequestType == 0x00 && bRequest == 0x09, bmRequestType == 0x01 && bRequest == 0x0b:
		r.stop(nil, true)
	case bmRequestType == 0x02 && bRequest == 0x01 && wValue == 0x00:
		endpoint := uint8(wIndex)
		r.stop(&endpoint, true)
	case r.policy.ftdi && bmRequestType == 0x40 && bRequest == 0x00 && (wValue == 0 || wValue == 1):
		endpoint := ftdiPortINEndpoint(wIndex)
		r.stop(&endpoint, true)
	}
}

// ftdiPortINEndpoint maps the port an FTDI vendor request addresses (wIndex 0
// or 1 is A, 2 is B, ...) to that port's bulk IN endpoint (0x81, 0x83, ...), so
// purging one port of a multi-port chip leaves the others' data alone.
func ftdiPortINEndpoint(wIndex uint16) uint8 {
	port := uint8(wIndex)
	if port == 0 {
		port = 1
	}
	return 0x81 + 2*(port-1)
}

// stop ends the readers on endpoint (every endpoint when nil), aborting the
// device read each has posted; flush also drops what they queued. Readers
// restart with the next client submit.
func (r *sessionReadAhead) stop(endpoint *uint8, flush bool) {
	if r == nil {
		return
	}
	type waitTarget struct {
		endpoint uint8
		done     chan struct{}
	}
	var targets []waitTarget
	r.access.Lock()
	for address, state := range r.endpoints {
		if endpoint != nil && address != *endpoint {
			continue
		}
		if state.running {
			state.stopping = true
			targets = append(targets, waitTarget{endpoint: address, done: state.readerDone})
		}
	}
	r.access.Unlock()

	for _, target := range targets {
		r.awaitReader(target.endpoint, target.done)
	}

	r.access.Lock()
	for address, state := range r.endpoints {
		if endpoint != nil && address != *endpoint {
			continue
		}
		state.stopping = false
		if flush {
			state.queue = nil
			state.queueBytes = 0
			state.statusQueue = false
		}
		// A submit that waited while the reader stopped starts the next one.
		select {
		case state.wake <- struct{}{}:
		default:
		}
	}
	r.access.Unlock()
}

// requeue puts back the transfer take handed to a submit the client unlinked
// before its response went out, so the bytes reach the next submit.
func (r *sessionReadAhead) requeue(endpoint uint8, response SubmitResponse) {
	if r == nil || len(response.Buffer) == 0 && response.Status == usbipStatusECONNRESET {
		return
	}
	r.access.Lock()
	defer r.access.Unlock()
	state := r.endpoints[endpoint]
	if state == nil || r.closed {
		return
	}
	state.queue = append([]readAheadResult{{status: response.Status, data: response.Buffer}}, state.queue...)
	state.queueBytes += len(response.Buffer)
	if response.Status != 0 {
		state.statusQueue = true
	}
}

// awaitReader aborts the endpoint until the reader's read returns. VBoxUSB
// can drop an abort that races the read's entry into the driver, hence the
// repeat; the bound keeps a wedged driver from hanging the session.
func (r *sessionReadAhead) awaitReader(endpoint uint8, done chan struct{}) {
	deadline := time.NewTimer(readAheadStopTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := r.session.engine.AbortEndpoint(endpoint)
		if err != nil {
			r.session.logger.Debug("read-ahead abort endpoint ", fmt.Sprintf("0x%02x", endpoint), ": ", err)
		}
		select {
		case <-done:
			return
		case <-deadline.C:
			r.session.logger.Warn("read-ahead reader on endpoint ", fmt.Sprintf("0x%02x", endpoint), " did not stop")
			return
		case <-ticker.C:
		}
	}
}

// close fails every waiting submit and stops the readers; the session calls it
// before closing the engine.
func (r *sessionReadAhead) close() {
	if r == nil {
		return
	}
	r.access.Lock()
	r.closed = true
	for _, state := range r.endpoints {
		select {
		case state.wake <- struct{}{}:
		default:
		}
	}
	r.access.Unlock()
	r.stop(nil, true)
}
