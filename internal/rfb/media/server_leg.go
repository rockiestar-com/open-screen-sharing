package media

import (
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"
)

const (
	pendingLimit      = 8
	pendingLifetime   = 5 * time.Second
	sentReportSlots   = 8
	maxServerOutputs  = 256
	legReadRetryDelay = 50 * time.Millisecond
)

type serverActive struct {
	ssrc         uint32
	receiverSSRC uint32
	generation   uint64
}

type serverOutput struct {
	replay        replayState
	generation    uint64
	offset        uint16
	started       bool
	highest       uint64
	based         bool
	base          uint64
	deviceBase    uint16
	lastSequence  uint16
	lastTimestamp uint32
	lastSentAt    time.Time
	packets       uint32
	octets        uint32
}

type pendingDatagram struct {
	data []byte
	from netip.AddrPort
	at   time.Time
}

// ScreensharingAgent writes the same port number into the local and the
// remote address of each media socket, then binds the socket to the local
// address and connects it to the remote one (ScreensharingAgent 756.36.5.2,
// macOS 27.0.1 26A434, UUID DA1CD9BA-97A1-3758-B425-00FA34B428A3,
// -[SSUDPSender sendToRemoteAddress:srcAddress:startingUDPPort:audioEncryptionKeyViewerToServer:audioEncryptionKeyServerToViewer:video1EncryptionKeyViewerToServer:video1EncryptionKeyServerToViewer:video2EncryptionKeyViewerToServer:video2EncryptionKeyServerToViewer:sessionID:supports60FPS:sendCursor:avcClientName:]
// 0x100008f50..0x100008f58, 0x1000092c8..0x1000092d8, 0x100009534..0x100009544,
// sub_100009aa8 0x100009b2c..0x100009b4c; ScreensharingAgent 766.8,
// macOS 27.2 26B5101f, UUID D8791A89-6EDB-3A0A-A720-EC3F5FB1AD4F,
// -[SSUDPSender sendToRemoteAddress:srcAddress:startingUDPPort:audioEncryptionKeyViewerToServer:audioEncryptionKeyServerToViewer:video1EncryptionKeyViewerToServer:video1EncryptionKeyServerToViewer:video2EncryptionKeyViewerToServer:video2EncryptionKeyServerToViewer:sessionID:supports60FPS:sendCursor:avcClientName:]
// 0x100008f50..0x100008f58, 0x1000092c8..0x1000092d8, 0x100009534..0x100009544,
// sub_100009aa8 0x100009b2c..0x100009b4c).
type serverLeg struct {
	name      string
	conn      *net.UDPConn
	port      uint16
	clockRate float64
	logger    *slog.Logger

	access        sync.Mutex
	initialTarget netip.AddrPort
	target        netip.AddrPort
	latched       bool
	peer          netip.AddrPort
	writer        N.ConnectedPacketBatchWriter
	pending       []pendingDatagram
	clientKey     [masterKeyLength]byte
	serverKey     [masterKeyLength]byte
	receiver      *srtcpReceiver
	sender        *srtpSender
	reports       *srtcpSender
	outputs       map[uint32]*serverOutput
	active        *serverActive
	held          uint64
	sentReports   [sentReportSlots]sentReport
	sentNext      int
	deviceReports map[uint32]*deviceReport
	clientRTT     time.Duration
	timing        serverTiming
}

func newServerLeg(name string, conn *net.UDPConn, clockRate float64, initial netip.Addr, logger *slog.Logger) *serverLeg {
	port := uint16(conn.LocalAddr().(*net.UDPAddr).Port)
	leg := &serverLeg{
		name:          name,
		conn:          conn,
		port:          port,
		clockRate:     clockRate,
		logger:        logger,
		outputs:       make(map[uint32]*serverOutput),
		deviceReports: make(map[uint32]*deviceReport),
	}
	if initial.IsValid() {
		leg.initialTarget = netip.AddrPortFrom(initial, port)
		leg.target = leg.initialTarget
	}
	return leg
}

func (l *serverLeg) setKeys(stream *serverOfferStream, now time.Time) error {
	l.access.Lock()
	defer l.access.Unlock()
	if l.receiver != nil && l.clientKey == stream.clientKey && l.serverKey == stream.serverKey {
		return l.checkPendingLocked(now)
	}
	l.clientKey, l.serverKey = stream.clientKey, stream.serverKey
	l.receiver = newSRTCPReceiver(stream.clientKey[:])
	l.sender = newSRTPSender(stream.serverKey[:])
	l.reports = newSRTCPSender(stream.serverKey[:])
	clear(l.outputs)
	l.sentReports = [sentReportSlots]sentReport{}
	l.clientRTT = 0
	l.target, l.latched = l.initialTarget, false
	if l.peer.IsValid() {
		l.target = l.peer
	}
	return l.checkPendingLocked(now)
}

func (l *serverLeg) checkPendingLocked(now time.Time) error {
	pending := l.pending
	l.pending = nil
	for _, datagram := range pending {
		if now.Sub(datagram.at) > pendingLifetime {
			continue
		}
		if _, err := l.receiver.unprotect(datagram.data); err != nil {
			continue
		}
		if _, err := l.latchLocked(datagram.from); err != nil {
			return err
		}
	}
	return nil
}

func (l *serverLeg) keepPendingLocked(datagram []byte, from netip.AddrPort, now time.Time) {
	if len(l.pending) >= pendingLimit {
		l.pending = append(l.pending[:0], l.pending[1:]...)
	}
	l.pending = append(l.pending, pendingDatagram{data: append([]byte(nil), datagram...), from: from, at: now})
}

func (l *serverLeg) latchLocked(from netip.AddrPort) (bool, error) {
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	if l.latched && l.target == from {
		return false, nil
	}
	if l.peer != from {
		writer, err := connectLeg(l.conn, from)
		if err != nil {
			return false, E.Cause(err, "connect the ", l.name, " socket to ", from)
		}
		l.peer, l.writer = from, writer
	}
	previous := l.target
	l.target, l.latched = from, true
	l.logger.Debug("media client address", "leg", l.name, "address", from, "previous", previous)
	held := l.held
	l.held = 0
	return held > 0, nil
}

func (l *serverLeg) currentTarget() netip.AddrPort {
	l.access.Lock()
	defer l.access.Unlock()
	return l.target
}

func (l *serverLeg) activate(active *serverActive) {
	l.access.Lock()
	l.active = active
	l.held = 0
	l.startTimingLocked(time.Now())
	l.access.Unlock()
}

func (l *serverLeg) deactivate() {
	l.access.Lock()
	l.active = nil
	l.access.Unlock()
}

func (l *serverLeg) outputLocked(ssrc uint32, generation uint64, sequence uint16) *serverOutput {
	output := l.outputs[ssrc]
	if output == nil {
		if len(l.outputs) >= maxServerOutputs {
			return nil
		}
		output = &serverOutput{generation: generation}
		l.outputs[ssrc] = output
		return output
	}
	if output.generation != generation {
		output.generation = generation
		output.based = false
		if output.started {
			output.offset = output.lastSequence + 1 - sequence
		}
	}
	return output
}

func (l *serverLeg) protectLocked(packet []byte, header rtpPacket, generation uint64, now time.Time) ([]byte, bool) {
	if l.active == nil || l.active.generation != generation {
		return nil, false
	}
	if !l.target.IsValid() {
		l.held++
		return nil, false
	}
	output := l.outputLocked(header.ssrc, generation, header.sequence)
	if output == nil {
		return nil, false
	}
	sequence := header.sequence + output.offset
	binary.BigEndian.PutUint16(packet[2:], sequence)
	index := output.replay.estimateIndex(sequence)
	if !output.replay.check(index) {
		return nil, false
	}
	output.replay.accept(index)
	if !output.based {
		output.based = true
		output.base = index
		output.deviceBase = header.sequence
	}
	if !output.started || index > output.highest {
		output.highest = index
	}
	output.started = true
	output.lastSequence = sequence
	output.lastTimestamp = header.timestamp
	output.lastSentAt = now
	output.packets++
	output.octets += uint32(len(packet) - header.payloadOffset)
	return l.sender.protect(packet, header.payloadOffset, index), true
}

func (l *serverLeg) send(datagrams []*buf.Buffer, target netip.AddrPort, writer N.ConnectedPacketBatchWriter) {
	var err error
	if writer != nil {
		err = writer.WriteConnectedPacketBatch(datagrams)
	} else {
		for _, datagram := range datagrams {
			if err == nil {
				_, err = l.conn.WriteToUDPAddrPort(datagram.Bytes(), target)
			}
			datagram.Release()
		}
	}
	if err == nil || errors.Is(err, net.ErrClosed) {
		return
	}
	l.access.Lock()
	l.timing.interval.sendErrors++
	l.timing.interval.lastSendError = err
	l.access.Unlock()
}

func (l *serverLeg) receive(handle func(leg *serverLeg, datagram []byte, from netip.AddrPort, now time.Time)) {
	buffer := make([]byte, maxDatagram)
	for {
		n, from, err := l.conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if errors.Is(err, syscall.ECONNREFUSED) {
				continue
			}
			l.logger.Debug("media leg read", "leg", l.name, "error", err)
			time.Sleep(legReadRetryDelay)
			continue
		}
		if !isRTCP(buffer[:n]) {
			continue
		}
		handle(l, buffer[:n], from, time.Now())
	}
}
