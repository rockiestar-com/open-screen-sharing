package deviceserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/display"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/imagemounter"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/pasteboard"
	"github.com/nekohasekai/open-screen-sharing/internal/rfb"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
)

// The Developer Disk Image registers this RemoteService for /System/Developer/usr/libexec/dtremotedisplayd
// (DDI 27A9266e, Library/LaunchDaemons/com.apple.coredevice.dtremotedisplayd.plist).
const displayService = "com.apple.coredevice.displayservice"

var (
	errRerouted       = E.New("a new connection to the device would take a different route; reconnecting")
	errRetryRequested = E.New("a new connection to the device was requested")
)

var retryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second}

const (
	stableConnection    = 30 * time.Second
	displayPollInterval = 5 * time.Second
	routeCheckInterval  = 5 * time.Second
)

type State int

const (
	StateStopped State = iota
	StateConnecting
	StatePreparing
	StateRunning
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateConnecting:
		return "connecting"
	case StatePreparing:
		return "preparing"
	case StateRunning:
		return "running"
	case StateFailed:
		return "failed"
	}
	return "State(" + strconv.Itoa(int(s)) + ")"
}

type Observer interface {
	StateChanged(state State, err error)
	DeviceConnected(info coredevice.DeviceInfo)
	ViewerConnected(session *rfb.ServerSession)
	ViewerDisconnected(session *rfb.ServerSession, err error)
	StreamingChanged(active bool)
}

type Options struct {
	Connect  func(ctx context.Context) (*coredevice.Device, error)
	Logger   *slog.Logger
	Observer Observer

	Listen string
	RFB    rfb.ServerOptions

	AutoUnlockPasscode     string
	AutoUnlockPasscodeType PasscodeType
	// BLSBacklightState: 0 off, 1 inactiveOn, 2 activeOn, 3 dimmed (BacklightServices
	// 24A446, NSStringFromBLSBacklightState 0x22582161c, table 0x268a47640); -1 unavailable.
	BacklightState func() int32

	DiskImageDirectory string
}

type Server struct {
	options  Options
	logger   *slog.Logger
	rfb      *rfb.Server
	notifier *notifier
	streams  *streamManager
	input    *inputWorker
	clip     *clipboardBridge
	buttons  buttonPresser

	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	retry       chan struct{}
	displayWake chan struct{}

	access        sync.Mutex
	started       bool
	closed        bool
	state         State
	stateErr      error
	closeErr      error
	attached      *attachment
	attemptCancel context.CancelCauseFunc
	attachWake    chan struct{}
	screen        rfb.ServerScreen
	display       displayGeometry
	videoProblem  error

	listener net.Listener
	relisted bool
}

type attachment struct {
	device            *coredevice.Device
	display           *display.Client
	pasteboard        *pasteboard.Client
	ctx               context.Context
	owner             context.Context
	cancel            context.CancelFunc
	notificationsDone chan error
}

func New(options Options) (*Server, error) {
	if err := validateAutoUnlock(options); err != nil {
		return nil, err
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	host, _, err := net.SplitHostPort(options.Listen)
	if err != nil {
		return nil, E.Cause(err, "deviceserver: parse listen address")
	}
	var ip net.IP
	if host != "" {
		ip = net.ParseIP(host)
		if ip == nil {
			return nil, E.New("deviceserver: listen address ", strconv.Quote(host), " is not an IP address")
		}
	}
	options.RFB.Media.ListenUDP = func(port uint16) (*net.UDPConn, error) {
		return net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: int(port)})
	}
	s := &Server{
		options:     options,
		logger:      logger,
		done:        make(chan struct{}),
		retry:       make(chan struct{}, 1),
		displayWake: make(chan struct{}, 1),
		attachWake:  make(chan struct{}),
	}
	rfbOptions := options.RFB
	rfbOptions.Handler = (*viewerHandler)(s)
	rfbOptions.Logger = logger.With("component", "rfb")
	rfbOptions.SystemCommands = []rfb.SystemCommand{
		rfb.SystemCommandLaunchpad, rfb.SystemCommandShowDesktop,
		rfb.SystemCommandMissionControl, rfb.SystemCommandAppWindows,
	}
	s.rfb, err = rfb.NewServer(rfbOptions)
	if err != nil {
		return nil, err
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.notifier = newNotifier()
	s.streams = newStreamManager(logger.With("component", "stream"), s.streamingChanged)
	s.input = newInputWorker(s, logger.With("component", "input"))
	s.clip = newClipboardBridge(s, logger.With("component", "clipboard"))
	return s, nil
}

func (s *Server) Start() error {
	s.access.Lock()
	defer s.access.Unlock()
	if s.closed {
		return E.New("deviceserver: closed")
	}
	if s.started {
		return E.New("deviceserver: already started")
	}
	s.started = true
	go s.run()
	return nil
}

func (s *Server) Retry() {
	s.access.Lock()
	cancel := s.attemptCancel
	select {
	case s.retry <- struct{}{}:
	default:
	}
	s.access.Unlock()
	if cancel != nil {
		cancel(errRetryRequested)
	}
}

func (s *Server) Close() error {
	s.access.Lock()
	if s.closed {
		s.access.Unlock()
		<-s.done
		return s.closeErr
	}
	s.closed = true
	started := s.started
	s.access.Unlock()
	s.cancel()
	_ = s.rfb.Close()
	if started {
		<-s.done
	} else {
		close(s.done)
	}
	s.stopped(s.closeErr)
	return s.closeErr
}

func (s *Server) stopped(err error) {
	s.access.Lock()
	changed := s.state != StateStopped || !errors.Is(s.stateErr, err) || !errors.Is(err, s.stateErr)
	s.state, s.stateErr = StateStopped, err
	s.access.Unlock()
	var last func()
	if changed {
		s.logger.Info("state", "state", StateStopped, "error", err)
		last = func() { s.options.Observer.StateChanged(StateStopped, err) }
	}
	s.notifier.close(last)
}

func (s *Server) setState(state State, err error) {
	s.access.Lock()
	if s.state == state && errors.Is(err, s.stateErr) && errors.Is(s.stateErr, err) {
		s.access.Unlock()
		return
	}
	s.state, s.stateErr = state, err
	s.access.Unlock()
	if err != nil {
		s.logger.Info("state", "state", state, "problem", ProblemOf(err), "error", err)
	} else {
		s.logger.Info("state", "state", state)
	}
	s.notify(func(observer Observer) { observer.StateChanged(state, err) })
}

func (s *Server) notify(call func(Observer)) {
	s.notifier.post(func() { call(s.options.Observer) })
}

func (s *Server) streamingChanged(active bool) {
	s.logger.Info("device video", "running", active)
	s.notify(func(observer Observer) { observer.StreamingChanged(active) })
}

func (s *Server) run() {
	defer close(s.done)
	inputContext, stopInput := context.WithCancel(context.Background())
	go s.input.run(inputContext)
	defer func() {
		stopInput()
		<-s.input.done
	}()
	failures := 0
	var lastErr error
	for s.ctx.Err() == nil {
		select {
		case <-s.retry:
		default:
		}
		s.setState(StateConnecting, lastErr)
		var connected time.Time
		err := func() error {
			ctx, cancel := context.WithCancelCause(s.ctx)
			defer cancel(nil)
			s.access.Lock()
			s.attemptCancel = cancel
			s.access.Unlock()
			device, err := s.options.Connect(ctx)
			s.access.Lock()
			s.attemptCancel = nil
			s.access.Unlock()
			if err != nil {
				if ctx.Err() != nil {
					return context.Cause(ctx)
				}
				return err
			}
			if err := s.ctx.Err(); err != nil {
				_ = device.Close()
				return err
			}
			connected = time.Now()
			return s.serveDevice(device)
		}()
		if s.ctx.Err() != nil {
			return
		}
		switch {
		case E.IsMulti(err, errRemounted, errRerouted, errRetryRequested):
			lastErr = nil
			failures = 0
			continue
		case stopping(err):
			s.fail(err)
			return
		}
		if connected.IsZero() {
			s.logger.Info("connect to the device", "error", err)
		} else {
			s.logger.Info("device connection ended", "error", err)
		}
		lastErr = err
		if !connected.IsZero() && time.Since(connected) >= stableConnection {
			failures = 0
		}
		if !s.wait(retryDelays[min(failures, len(retryDelays)-1)]) {
			return
		}
		failures++
	}
}

func stopping(err error) bool {
	switch ProblemOf(err) {
	case ProblemPairingRejected, ProblemDeveloperModeOff, ProblemNoDiskImage, ProblemDiskImage,
		ProblemListen, ProblemUnsupported, ProblemLoopbackAddress:
		return true
	}
	return false
}

func (s *Server) fail(err error) {
	s.logger.Error("device server stopped", "problem", ProblemOf(err), "error", err)
	_ = s.rfb.Close()
	s.setState(StateFailed, err)
}

func (s *Server) wait(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-s.ctx.Done():
		return false
	case <-s.retry:
		return true
	case <-timer.C:
		return true
	}
}

func (s *Server) serveDevice(device *coredevice.Device) error {
	defer device.Close()
	owner, cancelOwner := context.WithCancelCause(context.Background())
	defer cancelOwner(context.Canceled)
	go func() {
		select {
		case <-device.Done():
			cancelOwner(device.Err())
		case <-owner.Done():
		}
	}()
	info := device.Info()
	s.logger.Info("device connected", "udid", info.UDID, "product", info.ProductType, "os", info.OSVersion, "build", info.BuildVersion)
	s.notify(func(observer Observer) { observer.DeviceConnected(info) })
	s.setModel(info.ProductType)
	if major, ok := majorVersion(info.OSVersion); ok && major < 27 {
		return newError(ProblemUnsupported, "iOS ", info.OSVersion, " cannot share its screen; it needs iOS 27 or later")
	}
	logger := s.logger.With("component", "device")
	s.setState(StatePreparing, nil)
	err := s.prepareDiskImage(s.ctx, device, imagemounter.New(device, logger.With("service", "imagemounter")))
	if err != nil {
		return lostOr(device, err)
	}
	if !device.HasService(displayService) {
		return newError(ProblemUnsupported, "the device does not offer ", displayService, " although its disk image is mounted")
	}
	displayClient := display.New(owner, device, logger.With("service", "display"))
	state, err := displayClient.Displays(s.ctx)
	if err != nil {
		return lostOr(device, E.Cause(err, "read the device's displays"))
	}
	primary, found := primaryDisplay(state)
	if !found {
		return lostOr(device, E.New("the device lists no usable screen among its ", len(state), " displays"))
	}
	s.updateDisplay(primary)
	err = s.listen()
	if err != nil {
		return err
	}
	a := s.attach(&attachment{
		device:  device,
		display: displayClient,
		owner:   owner,
	})
	s.setRunning()
	rerouted := s.watchRoute(device)
	if err := s.detach(a); err != nil {
		s.access.Lock()
		if s.closed {
			s.closeErr = err
		}
		s.access.Unlock()
		return err
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if rerouted != nil {
		return rerouted
	}
	return &Error{Problem: ProblemNoTunnel, Err: E.Cause(device.Err(), "device connection lost")}
}

func (s *Server) watchRoute(device *coredevice.Device) error {
	local, remote := device.TransportAddr()
	var tick <-chan time.Time
	if local.IsValid() && remote.IsValid() {
		ticker := time.NewTicker(routeCheckInterval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-device.Done():
			return nil
		case <-s.ctx.Done():
			return nil
		case <-tick:
		}
		conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(remote))
		if err != nil {
			continue
		}
		current := M.AddrPortFromNet(conn.LocalAddr()).Addr().Unmap()
		_ = conn.Close()
		if current != local.Addr().Unmap() {
			s.logger.Info("device route changed", "from", local.Addr(), "to", current)
			return errRerouted
		}
	}
}

func lostOr(device *coredevice.Device, err error) error {
	select {
	case <-device.Done():
		return &Error{Problem: ProblemNoTunnel, Err: E.Cause(device.Err(), "device connection lost")}
	default:
		return err
	}
}

func (s *Server) setRunning() {
	s.access.Lock()
	problem := s.videoProblem
	s.access.Unlock()
	s.setState(StateRunning, problem)
}

func (s *Server) setVideoProblem(err error) {
	s.access.Lock()
	changed := (err == nil) != (s.videoProblem == nil)
	s.videoProblem = err
	running := s.state == StateRunning
	s.access.Unlock()
	if running && (changed || err != nil) {
		s.setState(StateRunning, err)
	}
}

// When the screen locks while the app is suspended, iOS defuncts the app's sockets.
// A defunct listener stays open but never reports a connection or an error.
func (s *Server) listen() error {
	if s.listener == nil {
		media := s.options.RFB.Media
		for _, port := range []uint16{media.VideoPort, media.AudioPort} {
			conn, err := media.ListenUDP(port)
			if err != nil {
				return &Error{Problem: ProblemListen, Err: E.Cause(err, "listen on media port ", port)}
			}
			_ = conn.Close()
		}
	} else {
		_ = s.listener.Close()
	}
	listener, err := net.Listen("tcp", s.options.Listen)
	if err != nil {
		return &Error{Problem: ProblemListen, Err: err}
	}
	s.listener = listener
	s.logger.Info("listening", "address", listener.Addr().String())
	go func() {
		err := s.rfb.Serve(listener)
		if err != nil && !E.IsMulti(err, rfb.ErrServerClosed, net.ErrClosed) {
			s.logger.Error("serve", "error", err)
		}
	}()
	return nil
}

func (s *Server) attach(a *attachment) *attachment {
	a.ctx, a.cancel = context.WithCancel(s.ctx)
	a.pasteboard = pasteboard.New(a.ctx, a.device, s.logger.With("component", "device", "service", "pasteboard"))
	s.access.Lock()
	s.attached = a
	close(s.attachWake)
	s.attachWake = make(chan struct{})
	s.access.Unlock()
	go s.pollDisplay(a)
	if a.device.HasService("com.apple.accessibility.axAuditDaemon.remoteAXService") {
		a.notificationsDone = make(chan error, 1)
		go func() { a.notificationsDone <- s.dismissScreenViewingNotifications(a) }()
	}
	s.clip.update()
	return a
}

func (s *Server) detach(a *attachment) error {
	s.access.Lock()
	s.attached = nil
	close(s.attachWake)
	s.attachWake = make(chan struct{})
	s.access.Unlock()
	a.cancel()
	s.clip.update()
	inputErr := s.input.forgetAndWait()
	buttonErr := s.buttons.close()
	streamErr := s.streams.stopDevice(a.device)
	var notificationErr error
	if a.notificationsDone != nil {
		notificationErr = <-a.notificationsDone
	}
	return E.Errors(inputErr, buttonErr, streamErr, notificationErr)
}

func (s *Server) currentDevice() *attachment {
	s.access.Lock()
	defer s.access.Unlock()
	return s.attached
}

func (s *Server) waitDevice(ctx context.Context) (*attachment, error) {
	for {
		s.access.Lock()
		a, wake := s.attached, s.attachWake
		s.access.Unlock()
		if a != nil {
			return a, nil
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, E.Cause(ctx.Err(), "the device is not connected")
		case <-s.ctx.Done():
			return nil, E.New("the server is closing")
		}
	}
}

func (s *Server) pollDisplay(a *attachment) {
	timer := time.NewTimer(displayPollInterval)
	defer timer.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-s.displayWake:
		case <-timer.C:
			if !s.streams.videoRunning() {
				timer.Reset(displayPollInterval)
				continue
			}
		}
		state, err := a.display.Displays(a.ctx)
		if err != nil {
			if a.ctx.Err() == nil {
				s.logger.Debug("read the device's displays", "error", err)
			}
		} else if primary, found := primaryDisplay(state); found {
			s.updateDisplay(primary)
		}
		timer.Reset(displayPollInterval)
	}
}

func (s *Server) checkDisplay() {
	select {
	case s.displayWake <- struct{}{}:
	default:
	}
}

func (s *Server) updateDisplay(info display.DisplayInfo) {
	s.access.Lock()
	previous := s.display
	s.display = geometryOf(info)
	changed := previous.Width != info.Width || previous.Height != info.Height
	if changed {
		// In mode 7 the device produced 2420×1668 visible video in a 2432×1680 coded frame;
		// mode 5 exposed the full 2432×1680 frame (observed on iPad17,1, iPadOS 27.2 24B5099f).
		screen := rfb.ServerScreen{Width: info.Width, Height: info.Height, Scale: float64(max(info.PointScale, 1)), Model: s.screen.Model}
		changed = screen != s.screen
		s.screen = screen
	}
	s.access.Unlock()
	if previous.Orientation != info.CurrentOrientation && previous.Orientation != "" {
		s.logger.Info("screen orientation", "orientation", info.CurrentOrientation, "size", fmt.Sprintf("%dx%d", info.Width, info.Height))
	}
	if changed {
		s.logger.Info("screen size changed", "width", info.Width, "height", info.Height)
		s.rfb.ScreenChanged()
	}
}

func (s *Server) setModel(model string) {
	s.access.Lock()
	changed := s.screen.Model != model
	s.screen.Model = model
	s.access.Unlock()
	if changed {
		s.rfb.ScreenChanged()
	}
}

func (s *Server) videoSize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	s.access.Lock()
	changed := s.screen.Width != width || s.screen.Height != height
	s.screen.Width, s.screen.Height = width, height
	s.access.Unlock()
	if changed {
		s.logger.Info("the video's size differs from the screen's", "width", width, "height", height)
		go s.rfb.ScreenChanged()
	}
}

func (s *Server) geometry() screenGeometry {
	s.access.Lock()
	defer s.access.Unlock()
	return screenGeometry{
		Width:   s.screen.Width,
		Height:  s.screen.Height,
		Scale:   s.screen.Scale,
		Density: s.screen.Density(),
		Turns:   touchTurns(s.display, s.screen.Width, s.screen.Height),
	}
}

func majorVersion(version string) (int, bool) {
	major, _, _ := strings.Cut(version, ".")
	value, err := strconv.Atoi(major)
	return value, err == nil
}

type notifier struct {
	access sync.Mutex
	queue  []func()
	wake   chan struct{}
	closed bool
}

func newNotifier() *notifier {
	n := &notifier{wake: make(chan struct{}, 1)}
	go n.run()
	return n
}

func (n *notifier) post(call func()) {
	n.access.Lock()
	if n.closed {
		n.access.Unlock()
		return
	}
	n.queue = append(n.queue, call)
	n.access.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *notifier) run() {
	for {
		n.access.Lock()
		queue := n.queue
		n.queue = nil
		closed := n.closed
		n.access.Unlock()
		for _, call := range queue {
			call()
		}
		if closed && len(queue) == 0 {
			return
		}
		if len(queue) == 0 {
			<-n.wake
		}
	}
}

func (n *notifier) close(last func()) {
	n.access.Lock()
	if last != nil {
		n.queue = append(n.queue, last)
	}
	n.closed = true
	n.access.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}
