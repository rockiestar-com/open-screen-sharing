package rfb

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/nekohasekai/open-screen-sharing/internal/rfb/media"
	E "github.com/sagernet/sing/common/exceptions"
)

var ErrServerClosed = E.New("rfb: server closed")

const (
	signInFailureDelay   = time.Second
	acceptRetryLimit     = time.Second
	maxPendingHandshakes = 16
)

type ServerScreen struct {
	Width, Height int
	Scale         float64
	Model         string
}

func (s ServerScreen) Density() float64 {
	if s.Scale > 1 {
		return 2
	}
	return 1
}

type ServerHandler interface {
	Screen() ServerScreen
	SessionStarted(session *ServerSession)
	HandleEvent(session *ServerSession, event ViewerEvent)
	Clipboard(ctx context.Context) (*Clipboard, error)
	OpenVideo(ctx context.Context, request media.ServerVideoRequest) (media.ServerVideoSource, error)
	OpenAudio() (media.ServerAudioSource, error)
	SessionEnded(session *ServerSession, err error)
}

type ServerOptions struct {
	Name             string
	Password         string
	Username         string
	NoAuthentication bool
	Logger           *slog.Logger
	Handler          ServerHandler

	SystemCommands []SystemCommand

	Media media.ServerStreamConfig
}

type Server struct {
	options  ServerOptions
	logger   *slog.Logger
	handler  ServerHandler
	verifier *srpVerifier

	access          sync.Mutex
	closed          bool
	done            chan struct{}
	listeners       map[net.Listener]struct{}
	conns           map[net.Conn]struct{}
	sessions        map[*ServerSession]struct{}
	mediaOwner      *ServerSession
	nextID          uint64
	handshakes      int
	failureAnswerAt time.Time
	wg              sync.WaitGroup
}

func NewServer(options ServerOptions) (*Server, error) {
	if options.Password == "" && !options.NoAuthentication {
		return nil, E.New("rfb: the server needs a password")
	}
	s := &Server{
		options:   options,
		logger:    options.Logger,
		handler:   options.Handler,
		done:      make(chan struct{}),
		listeners: make(map[net.Listener]struct{}),
		conns:     make(map[net.Conn]struct{}),
		sessions:  make(map[*ServerSession]struct{}),
	}
	password := options.Password
	if options.NoAuthentication {
		password = hex.EncodeToString(randomBytes(32))
	}
	s.verifier = newSRPVerifier(password)
	return s, nil
}

func (s *Server) Serve(listener net.Listener) error {
	s.access.Lock()
	if s.closed {
		s.access.Unlock()
		_ = listener.Close()
		return ErrServerClosed
	}
	s.listeners[listener] = struct{}{}
	s.access.Unlock()
	defer func() {
		s.access.Lock()
		delete(s.listeners, listener)
		s.access.Unlock()
	}()
	var delay time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			s.access.Lock()
			closed := s.closed
			s.access.Unlock()
			if closed {
				return ErrServerClosed
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			delay = min(max(2*delay, 5*time.Millisecond), acceptRetryLimit)
			s.logger.Warn("accept", "error", err, "retry", delay)
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-s.done:
				timer.Stop()
				return ErrServerClosed
			}
			continue
		}
		delay = 0
		s.access.Lock()
		if s.closed {
			s.access.Unlock()
			_ = conn.Close()
			return ErrServerClosed
		}
		if s.handshakes >= maxPendingHandshakes {
			s.access.Unlock()
			s.logger.Debug("too many viewers signing in", "viewer", conn.RemoteAddr().String())
			_ = conn.Close()
			continue
		}
		s.handshakes++
		s.conns[conn] = struct{}{}
		s.nextID++
		id := s.nextID
		s.wg.Go(func() {
			s.serveConn(conn, id)
			s.access.Lock()
			delete(s.conns, conn)
			s.access.Unlock()
		})
		s.access.Unlock()
	}
}

func (s *Server) Close() error {
	s.access.Lock()
	if s.closed {
		s.access.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	listeners := slices.Collect(maps.Keys(s.listeners))
	conns := slices.Collect(maps.Keys(s.conns))
	s.access.Unlock()
	var err error
	for _, listener := range listeners {
		err = E.Errors(err, listener.Close())
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	s.wg.Wait()
	return err
}

func (s *Server) Sessions() []*ServerSession {
	s.access.Lock()
	defer s.access.Unlock()
	return slices.Collect(maps.Keys(s.sessions))
}

func (s *Server) ScreenChanged() {
	screen := s.handler.Screen()
	for _, session := range s.Sessions() {
		session.screenChanged(screen)
	}
}

func (s *Server) ClipboardChanged() {
	for _, session := range s.Sessions() {
		session.clipboardChanged()
	}
}

func (s *Server) endHandshake() {
	s.access.Lock()
	s.handshakes--
	s.access.Unlock()
}

func (s *Server) signInFailureWait(now time.Time) time.Duration {
	s.access.Lock()
	defer s.access.Unlock()
	answerAt := now.Add(signInFailureDelay)
	if next := s.failureAnswerAt.Add(signInFailureDelay); next.After(answerAt) {
		answerAt = next
	}
	s.failureAnswerAt = answerAt
	return answerAt.Sub(now)
}

func (s *Server) acceptsUserName(name string) bool {
	expected := s.options.Username
	return expected == "" || subtle.ConstantTimeCompare([]byte(name), []byte(expected)) == 1
}

func (s *Server) addSession(session *ServerSession) bool {
	s.access.Lock()
	defer s.access.Unlock()
	if s.closed {
		return false
	}
	s.sessions[session] = struct{}{}
	return true
}

func (s *Server) removeSession(session *ServerSession) {
	s.access.Lock()
	delete(s.sessions, session)
	if s.mediaOwner == session {
		s.mediaOwner = nil
	}
	s.access.Unlock()
}

func (s *Server) claimMedia(session *ServerSession) bool {
	s.access.Lock()
	defer s.access.Unlock()
	if s.mediaOwner != nil && s.mediaOwner != session {
		return false
	}
	s.mediaOwner = session
	return true
}

func (s *Server) releaseMedia(session *ServerSession) {
	s.access.Lock()
	if s.mediaOwner == session {
		s.mediaOwner = nil
	}
	s.access.Unlock()
}

func (s *Server) mediaTakenBy(session *ServerSession) bool {
	s.access.Lock()
	defer s.access.Unlock()
	return s.mediaOwner != nil && s.mediaOwner != session
}

func (s *Server) serveConn(conn net.Conn, id uint64) {
	tcpConn, isTCP := conn.(*net.TCPConn)
	if isTCP {
		_ = tcpConn.SetKeepAliveConfig(net.KeepAliveConfig{
			Enable:   true,
			Idle:     5 * time.Second,
			Interval: 5 * time.Second,
			Count:    3,
		})
	}
	session := newServerSession(s, conn, id)
	session.run()
}
