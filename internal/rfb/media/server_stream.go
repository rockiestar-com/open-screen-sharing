package media

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
)

const (
	maxSourceFailures = 3
	forwardBatchSize  = 32
	videoSendBuffer   = 1 << 20
	serverReadBuffer  = 256 << 10
)

type ServerStreamConfig struct {
	Logger              *slog.Logger
	InitialTarget       netip.Addr
	AllowLoopbackTarget bool
	VideoPort           uint16
	AudioPort           uint16
	ListenUDP           func(port uint16) (*net.UDPConn, error)
	OpenVideo           ServerVideoOpener
	OpenAudio           ServerAudioOpener
	Send                func(payload []byte) error
}

type ServerStream struct {
	config ServerStreamConfig
	logger *slog.Logger
	video  *serverLeg
	audio  *serverLeg
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	access          sync.Mutex
	closed          bool
	generation      uint64
	offered         bool
	renegotiateSize *[2]int
	cancelOpen      context.CancelFunc
	session         *serverSession
	failures        int
}

type serverSession struct {
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	video      ServerVideoSource
	audio      ServerAudioSource
	forwarded  atomic.Uint64
	wg         sync.WaitGroup
}

func (s *serverSession) close() {
	s.cancel()
	_ = s.video.Close()
	if s.audio != nil {
		_ = s.audio.Close()
	}
	s.wg.Wait()
}

func ListenServerStream(config ServerStreamConfig) (*ServerStream, error) {
	initial := config.InitialTarget.Unmap()
	if initial.IsLoopback() && !config.AllowLoopbackTarget {
		initial = netip.Addr{}
	}
	videoConn, err := config.ListenUDP(config.VideoPort)
	if err != nil {
		return nil, E.Cause(err, "media: listen on UDP port ", config.VideoPort)
	}
	_ = videoConn.SetReadBuffer(serverReadBuffer)
	_ = videoConn.SetWriteBuffer(videoSendBuffer)
	ctx, cancel := context.WithCancel(context.Background())
	s := &ServerStream{
		config: config,
		logger: config.Logger,
		ctx:    ctx,
		cancel: cancel,
		video:  newServerLeg("video", videoConn, videoClockRate, initial, config.Logger),
	}
	audioConn, err := config.ListenUDP(config.AudioPort)
	if err != nil {
		cancel()
		_ = videoConn.Close()
		return nil, E.Cause(err, "media: listen on UDP port ", config.AudioPort)
	}
	_ = audioConn.SetReadBuffer(serverReadBuffer)
	s.audio = newServerLeg("audio", audioConn, audioSampleRate, initial, config.Logger)
	for _, leg := range s.legs() {
		s.wg.Go(func() { leg.receive(s.handleClientDatagram) })
	}
	s.wg.Go(s.housekeep)
	return s, nil
}

func (s *ServerStream) legs() []*serverLeg {
	return []*serverLeg{s.video, s.audio}
}

func (s *ServerStream) Ports() (audio, video uint16) {
	return s.audio.port, s.video.port
}

func (s *ServerStream) PortsMessage() []byte {
	audio, video := s.Ports()
	return portsMessage(audio, video)
}

func (s *ServerStream) HandleOffer(message []byte) error {
	offer, err := parseOffer(message)
	s.access.Lock()
	defer s.access.Unlock()
	if err != nil {
		s.logger.Warn("media offer", "error", err)
		return s.config.Send(ErrorMessage(ServerErrorBadConfiguration, 0))
	}
	s.generation++
	generation := s.generation
	s.closeLaterLocked(s.detachLocked())
	now := time.Now()
	err = s.video.setKeys(&offer.Video, now)
	if err == nil {
		err = s.audio.setKeys(&offer.Audio, now)
	}
	if err != nil {
		s.logger.Warn("media offer", "error", err)
		return s.config.Send(ErrorMessage(ServerErrorBadConfiguration, 0))
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.cancelOpen = cancel
	s.offered = true
	s.logger.Debug("media offer", "video_mode", offer.Video.Mode, "audio_mode", offer.Audio.Mode,
		"width", offer.Video.Width, "height", offer.Video.Height, "frame_rate_60", offer.FrameRate60,
		"pixel_formats", offer.Video.PixelFormats, "maximum_bitrate", offer.Video.MaximumBitrate)
	s.wg.Go(func() { s.open(ctx, cancel, generation, offer) })
	return nil
}

func (s *ServerStream) closeLaterLocked(session *serverSession) {
	if session != nil {
		s.wg.Go(session.close)
	}
}

func (s *ServerStream) open(ctx context.Context, cancel context.CancelFunc, generation uint64, offer *serverOffer) {
	defer cancel()
	wantsAudio := offer.Audio.Present && offer.Audio.Mode == negotiatorModeRemoteDesktopSystemAudio
	video, err := s.config.OpenVideo(ctx, ServerVideoRequest{
		ClientSSRC:     offer.Video.SSRC,
		Mode:           VideoMode(offer.Video.Mode),
		TilesPerFrame:  offer.Video.TilesPerFrame,
		PixelFormats:   offer.Video.PixelFormats,
		MaximumBitrate: offer.Video.MaximumBitrate,
		Audio:          wantsAudio,
	})
	if err != nil {
		s.openFailed(generation, E.Cause(err, "open the device video"))
		return
	}
	var audio ServerAudioSource
	if wantsAudio {
		audio, err = s.config.OpenAudio()
		if err != nil {
			if !errors.Is(err, ErrAudioUnsupported) {
				s.logger.Warn("open the device sound", "error", err)
			}
			audio = nil
		}
	}
	info := video.Info()
	answer := serverAnswer{
		FrameRate60: info.FrameRate != 30,
		Video:       info,
		PixelFormat: answerPixelFormat(offer.Video.PixelFormats, info.PixelFormat),
	}
	var audioInfo ServerAudioInfo
	if audio != nil {
		audioInfo = audio.Info()
		answer.Audio = &audioInfo
	}
	payload, err := answerMessage(answer)
	closeSources := func() {
		_ = video.Close()
		if audio != nil {
			_ = audio.Close()
		}
	}
	if err != nil {
		closeSources()
		s.openFailed(generation, err)
		return
	}
	s.access.Lock()
	defer s.access.Unlock()
	if s.generation != generation {
		closeSources()
		return
	}
	s.cancelOpen = nil
	sessionContext, sessionCancel := context.WithCancel(s.ctx)
	session := &serverSession{
		generation: generation,
		ctx:        sessionContext,
		cancel:     sessionCancel,
		video:      video,
		audio:      audio,
	}
	s.session = session
	err = s.config.Send(payload)
	if err != nil {
		s.logger.Debug("send the media answer", "error", err)
	}
	s.video.activate(&serverActive{ssrc: info.SSRC, receiverSSRC: info.ReceiverSSRC, generation: generation})
	if audio != nil {
		s.audio.activate(&serverActive{ssrc: audioInfo.SSRC, receiverSSRC: audioInfo.ReceiverSSRC, generation: generation})
	}
	session.wg.Go(func() { s.forward(session, s.video, video.ReadPackets) })
	if audio != nil {
		session.wg.Go(func() { s.forward(session, s.audio, audio.ReadPackets) })
	}
	s.logger.Info("media stream started", "ssrc", info.SSRC, "width", info.Width, "height", info.Height,
		"sound", audio != nil, "target", s.video.currentTarget())
	if size := s.renegotiateSize; size != nil {
		s.renegotiateSize = nil
		if *size != [2]int{info.Width, info.Height} {
			previous, err := s.renegotiateLocked()
			s.closeLaterLocked(previous)
			if err != nil {
				s.logger.Debug("send the media ports", "error", err)
			}
		}
	}
}

func (s *ServerStream) openFailed(generation uint64, err error) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.generation != generation {
		return
	}
	s.cancelOpen = nil
	s.renegotiateSize = nil
	s.logger.Warn("media stream", "error", err)
	_ = s.config.Send(ErrorMessage(ServerErrorBadConfiguration, 0))
}

func (s *ServerStream) detachLocked() *serverSession {
	if s.cancelOpen != nil {
		s.cancelOpen()
		s.cancelOpen = nil
	}
	session := s.session
	s.session = nil
	if session == nil {
		return nil
	}
	for _, leg := range s.legs() {
		leg.deactivate()
	}
	session.cancel()
	return session
}

// ScreenSharing ignores a new ports message while its
// mediaStreamMessage1Received flag is set; it sets the flag after handling a
// ports message, and handling the answer clears it (ScreenSharing 776.27,
// macOS 27.0.1 26A434, UUID EC0B2C20-F816-3FCE-822D-F9403C09EB50,
// -[SSSession handleAVCMediaEncoding:] 0x1cc0e48dc..0x1cc0e495c,
// 0x1cc0e5c54..0x1cc0e5c5c, 0x1cc0e49d4..0x1cc0e49e8; ScreenSharing 786.3,
// macOS 27.2 26B5101f, UUID 21DF7A41-0D38-3DF3-B87A-8A6249686735,
// -[SSSession handleAVCMediaEncoding:] 0x22e52795c..0x22e5279dc,
// 0x22e528cd4..0x22e528cdc, 0x22e527a54..0x22e527a68).
func (s *ServerStream) Renegotiate(width, height int) error {
	s.access.Lock()
	if s.closed {
		s.access.Unlock()
		return os.ErrClosed
	}
	if s.cancelOpen != nil {
		s.renegotiateSize = &[2]int{width, height}
		s.access.Unlock()
		return nil
	}
	if s.session != nil {
		info := s.session.video.Info()
		if info.Width == width && info.Height == height {
			s.access.Unlock()
			return nil
		}
	}
	previous, err := s.renegotiateLocked()
	s.access.Unlock()
	if previous != nil {
		previous.close()
	}
	return err
}

func (s *ServerStream) renegotiateLocked() (*serverSession, error) {
	if !s.offered {
		return nil, nil
	}
	s.generation++
	previous := s.detachLocked()
	return previous, s.config.Send(s.PortsMessage())
}

func (s *ServerStream) Close() error {
	s.access.Lock()
	s.closed = true
	s.generation++
	previous := s.detachLocked()
	s.access.Unlock()
	s.cancel()
	if previous != nil {
		previous.close()
	}
	for _, leg := range s.legs() {
		_ = leg.conn.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *ServerStream) forward(session *serverSession, leg *serverLeg, read func(context.Context, []ServerPacket) (int, error)) {
	isVideo := leg == s.video
	batch := 1
	if isVideo {
		batch = forwardBatchSize
	}
	packets := make([]ServerPacket, batch)
	datagrams := make([]*buf.Buffer, 0, batch)
	var positioner *bandPositioner
	if isVideo {
		positioner = newBandPositioner(session.video.Info())
	}
	for {
		n, err := read(session.ctx, packets)
		if err != nil {
			s.sourceEnded(session, leg, err)
			return
		}
		now := time.Now()
		datagrams = datagrams[:0]
		leg.access.Lock()
		for _, received := range packets[:n] {
			arrived := now
			if !received.Arrival.IsZero() {
				arrived = now.Add(-now.Sub(received.Arrival))
			}
			if received.RTCP {
				leg.recordDeviceRTCPLocked(received.Buffer.Bytes(), now, arrived)
				received.Buffer.Release()
				continue
			}
			buffer := received.Buffer
			packet := buffer.Bytes()
			header, err := parseRTPHeader(packet)
			if err != nil {
				buffer.Release()
				continue
			}
			if isVideo {
				rewriteExtensionProfile(packet)
				if positioner != nil {
					packet = positioner.rewrite(packet, header)
				}
			}
			datagram, ok := leg.protectLocked(packet, header, session.generation, now)
			if !ok {
				buffer.Release()
				continue
			}
			if isVideo {
				leg.timePacketLocked(header, len(packet), arrived, now)
			}
			buffer.Truncate(len(datagram))
			datagrams = append(datagrams, buffer)
		}
		target, writer := leg.target, leg.writer
		leg.access.Unlock()
		if len(datagrams) == 0 {
			continue
		}
		leg.send(datagrams, target, writer)
		session.forwarded.Add(uint64(len(datagrams)))
	}
}

func (s *ServerStream) sourceEnded(session *serverSession, leg *serverLeg, err error) {
	if session.ctx.Err() != nil {
		return
	}
	s.access.Lock()
	if s.session != session {
		s.access.Unlock()
		return
	}
	if leg != s.video {
		s.logger.Warn("device sound stopped", "error", err)
		s.audio.deactivate()
		s.access.Unlock()
		return
	}
	if session.forwarded.Load() > 0 {
		s.failures = 0
	}
	s.failures++
	s.logger.Warn("device video stopped", "error", err, "failures", s.failures)
	if s.failures > maxSourceFailures {
		s.failures = 0
		s.generation++
		previous := s.detachLocked()
		_ = s.config.Send(ErrorMessage(ServerErrorBadConfiguration, 0))
		s.closeLaterLocked(previous)
		s.access.Unlock()
		return
	}
	previous, sendErr := s.renegotiateLocked()
	s.closeLaterLocked(previous)
	s.access.Unlock()
	if sendErr != nil {
		s.logger.Debug("send the media ports", "error", sendErr)
	}
}

func (s *ServerStream) handleClientDatagram(leg *serverLeg, datagram []byte, from netip.AddrPort, now time.Time) {
	leg.access.Lock()
	if leg.receiver == nil {
		leg.keepPendingLocked(datagram, from, now)
		leg.access.Unlock()
		return
	}
	packet, err := leg.receiver.unprotect(datagram)
	if err != nil {
		if errors.Is(err, errAuthentication) {
			leg.keepPendingLocked(datagram, from, now)
		}
		leg.access.Unlock()
		return
	}
	missed, err := leg.latchLocked(from)
	if err != nil {
		leg.access.Unlock()
		s.access.Lock()
		s.generation++
		s.closeLaterLocked(s.detachLocked())
		s.logger.Warn("media stream", "error", err)
		_ = s.config.Send(ErrorMessage(ServerErrorBadConfiguration, 0))
		s.access.Unlock()
		return
	}
	out, keyframe := leg.rewriteClientRTCPLocked(packet, now)
	leg.access.Unlock()
	if leg != s.video {
		keyframe = false
	} else if missed {
		keyframe = true
	}
	s.access.Lock()
	session := s.session
	s.access.Unlock()
	if session == nil {
		return
	}
	if leg == s.video {
		if len(out) > 0 {
			_ = session.video.WriteRTCP(out)
		}
		if keyframe {
			err = session.video.RequestKeyFrame()
			if err != nil {
				s.logger.Debug("request a keyframe from the device", "error", err)
			}
		}
	} else if session.audio != nil && len(out) > 0 {
		_ = session.audio.WriteRTCP(out)
	}
}

func (s *ServerStream) housekeep() {
	ticker := time.NewTicker(reportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			for _, leg := range s.legs() {
				leg.access.Lock()
				report, ok := leg.senderReportLocked(now)
				target, writer := leg.target, leg.writer
				leg.access.Unlock()
				if ok {
					leg.send([]*buf.Buffer{buf.As(report)}, target, writer)
				}
			}
			s.video.access.Lock()
			timing, ok := s.video.takeTimingLocked(now)
			s.video.access.Unlock()
			if ok {
				s.logger.Info("media timing", timing...)
			}
		}
	}
}
