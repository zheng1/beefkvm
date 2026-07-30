package ipmi

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// SOL is an active Serial-over-LAN session (IPMI 2.0 ch 15). It runs on its own
// RMCP+ session (separate UDP socket from the sensor/power client) and streams
// serial bytes both ways. The BMC's serial console must be routed to a payload
// channel and, on the target OS side, a serial getty / console must exist for
// there to be any data — SOL only carries whatever the target puts on the wire.
type SOL struct {
	s      *Session
	onData func([]byte) // called with each chunk of received serial data

	txMu  sync.Mutex // serialises outbound packets (outSeq + conn.Write)
	txSeq byte       // our outbound SOL packet sequence (1..15; 0 = ack-only)

	rxMu      sync.Mutex
	lastRxSeq byte // last delivered BMC packet seq, for duplicate suppression

	acks     chan byte // BMC ack-seq values observed by the read loop
	acksSeen int64     // atomic: count of BMC ACKs (proves TX reaches the BMC)
	done     chan struct{}
	readDone chan struct{}
	once     sync.Once
}

// AcksSeen returns how many SOL ACKs the BMC has sent for our transmitted
// packets — a nonzero count proves the console→BMC path is live even when the
// target emits nothing back.
func (sol *SOL) AcksSeen() int64 { return atomic.LoadInt64(&sol.acksSeen) }

// OpenSOL dials a fresh RMCP+ session, activates the SOL payload, and starts a
// receive loop delivering serial bytes to onData. Call Close to deactivate.
func OpenSOL(host, user, pass string, onData func([]byte)) (*SOL, error) {
	s, err := Dial(host, user, pass)
	if err != nil {
		return nil, err
	}
	// Activate Payload (NetFn App 0x06, cmd 0x48). data:
	//   [0] payload type (1 = SOL)
	//   [1] payload instance (1)
	//   [2] aux: [7]=encrypt, [6]=authenticate (this BMC forces encryption)
	//   [3..5] reserved
	cc, _, err := s.rawCall(netFnAppReq, 0x48, []byte{payloadSOL, 0x01, 0xC0, 0x00, 0x00, 0x00})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("ipmi: SOL activate: %w", err)
	}
	if cc != 0 {
		s.Close()
		return nil, fmt.Errorf("ipmi: SOL Activate Payload cc=0x%02x (%s)", cc, activateErr(cc))
	}
	sol := &SOL{
		s:        s,
		onData:   onData,
		acks:     make(chan byte, 16),
		done:     make(chan struct{}),
		readDone: make(chan struct{}),
	}
	go sol.readLoop()
	go sol.keepalive()
	return sol, nil
}

// activateErr decodes the SOL-specific completion codes for Activate Payload.
func activateErr(cc byte) string {
	switch cc {
	case 0x80:
		return "payload already active on another session"
	case 0x81:
		return "payload disabled"
	case 0x82:
		return "payload activation limit reached"
	case 0x83:
		return "cannot activate with encryption"
	case 0x84:
		return "cannot activate without encryption"
	default:
		return "activation failed"
	}
}

// readLoop reads UDP packets, delivers SOL serial data to onData, ACKs each
// data packet, and forwards BMC ack-seq values to the acks channel.
func (sol *SOL) readLoop() {
	defer close(sol.readDone)
	buf := make([]byte, 4096)
	for {
		select {
		case <-sol.done:
			return
		default:
		}
		_ = sol.s.conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		n, err := sol.s.conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		pt, _, _, payload, perr := parseRMCPPlus(buf[:n], sol.s.k1, sol.s.k2)
		if perr != nil || pt != payloadSOL || len(payload) < 4 {
			continue
		}
		seq := payload[0] & 0x0f
		ackSeq := payload[1] & 0x0f
		data := payload[4:]
		if ackSeq != 0 {
			atomic.AddInt64(&sol.acksSeen, 1)
			select {
			case sol.acks <- ackSeq:
			default:
			}
		}
		if seq != 0 && len(data) > 0 {
			sol.rxMu.Lock()
			dup := seq == sol.lastRxSeq
			sol.lastRxSeq = seq
			sol.rxMu.Unlock()
			if !dup && sol.onData != nil {
				sol.onData(data)
			}
			// ACK the received packet (accept all chars; ack-only, seq=0).
			cc := byte(len(data))
			if len(data) > 255 {
				cc = 255
			}
			_ = sol.sendPacket(0, seq, cc, nil)
		}
	}
}

// keepalive nudges the session so an idle SOL link doesn't time out. An
// ack-only SOL packet (seq=0, ack=0) is a valid no-op heartbeat.
func (sol *SOL) keepalive() {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-sol.done:
			return
		case <-t.C:
			_ = sol.sendPacket(0, 0, 0, nil)
		}
	}
}

// Write transmits serial bytes (keystrokes) to the target. Best-effort: if the
// BMC doesn't ACK, the user simply retypes — adequate for an interactive shell.
func (sol *SOL) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	sol.txMu.Lock()
	sol.txSeq++
	if sol.txSeq == 0 || sol.txSeq > 15 {
		sol.txSeq = 1
	}
	seq := sol.txSeq
	sol.txMu.Unlock()
	return sol.sendPacket(seq, 0, 0, data)
}

// sendPacket builds and sends one SOL payload packet under the tx mutex so the
// session sequence number and socket write stay consistent with the read loop's
// ACKs.
func (sol *SOL) sendPacket(seq, ackSeq, charCount byte, data []byte) error {
	pkt := make([]byte, 4+len(data))
	pkt[0] = seq
	pkt[1] = ackSeq
	pkt[2] = charCount
	pkt[3] = 0 // operation/status: no flush/break
	copy(pkt[4:], data)

	sol.txMu.Lock()
	defer sol.txMu.Unlock()
	seqNum := sol.s.outSeq
	sol.s.outSeq++
	out := buildRMCPPlus(payloadSOL, sol.s.sessionID, seqNum, pkt, sol.s.k1, sol.s.k2, newIV())
	_, err := sol.s.conn.Write(out)
	return err
}

// Close deactivates the SOL payload and tears the session down.
func (sol *SOL) Close() error {
	sol.once.Do(func() { close(sol.done) })
	// Let the read loop exit before issuing Deactivate (which reads a reply on
	// the same socket) to avoid two concurrent readers.
	select {
	case <-sol.readDone:
	case <-time.After(time.Second):
	}
	sol.txMu.Lock()
	_, _, _ = sol.s.rawCall(netFnAppReq, 0x49, []byte{payloadSOL, 0x01, 0x00, 0x00, 0x00, 0x00})
	sol.txMu.Unlock()
	return sol.s.Close()
}
