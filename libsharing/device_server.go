package libsharing

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/lockdown"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/vpnpeer"
	"github.com/nekohasekai/open-screen-sharing/internal/deviceserver"
	"github.com/nekohasekai/open-screen-sharing/internal/rfb"
	"github.com/nekohasekai/open-screen-sharing/internal/rfb/media"
)

const (
	DeviceServerStopped    int32 = 0
	DeviceServerConnecting int32 = 1
	DeviceServerPreparing  int32 = 2
	DeviceServerRunning    int32 = 3
	DeviceServerFailed     int32 = 4
)

const (
	DeviceErrorNone             int32 = 0
	DeviceErrorNoTunnel         int32 = 1
	DeviceErrorPairingRejected  int32 = 2
	DeviceErrorDeveloperModeOff int32 = 3
	DeviceErrorNoDiskImage      int32 = 4
	DeviceErrorDiskImage        int32 = 5
	DeviceErrorListen           int32 = 6
	DeviceErrorUnsupported      int32 = 7
	DeviceErrorScreenInUse      int32 = 8
	DeviceErrorPairing          int32 = 9
	DeviceErrorLoopbackAddress  int32 = 10
)

type DeviceServerOptions struct {
	PairingRecord          []byte
	RemotePairingPort      int32
	Port                   int32
	NoAuthentication       bool
	Username               string
	Password               string
	AutoUnlockPasscode     string
	AutoUnlockPasscodeType int32
	DiskImageDirectory     string
}

type DeviceServerHandler interface {
	OnStateChanged(state int32, errorCode int32, message string)
	OnViewerConnected(viewerID int64)
	OnViewerDisconnected(viewerID int64)
	BacklightState() int32
}

const defaultRemotePairingPort = 49152

type LoopbackAddress struct {
	LocalNetwork bool
}

func newLoopbackAddress(peer vpnpeer.Peer) *LoopbackAddress {
	return &LoopbackAddress{LocalNetwork: peer.LocalNetwork}
}

type LoopbackProbe struct {
	ctx    context.Context
	cancel context.CancelFunc
	port   uint16
}

func NewLoopbackProbe(remotePairingPort int32) *LoopbackProbe {
	port := uint16(remotePairingPort)
	if port == 0 {
		port = defaultRemotePairingPort
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &LoopbackProbe{ctx: ctx, cancel: cancel, port: port}
}

func (p *LoopbackProbe) Run() (*LoopbackAddress, error) {
	defer p.cancel()
	peer, err := vpnpeer.Find(p.ctx, p.port)
	if p.ctx.Err() != nil {
		return nil, p.ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return newLoopbackAddress(peer), nil
}

func (p *LoopbackProbe) Cancel() {
	p.cancel()
}

type DeviceServer struct {
	pairingPort atomic.Uint32
	port        int32
	loopback    atomic.Pointer[LoopbackAddress]
	server      *deviceserver.Server
}

func NewDeviceServer(options *DeviceServerOptions, handler DeviceServerHandler) (*DeviceServer, error) {
	file, err := parsePairingFile(options.PairingRecord)
	if err != nil {
		return nil, err
	}
	name := "iPhone"
	if file.remote != nil && file.remote.DeviceName != "" {
		name = file.remote.DeviceName
	}
	logger := newLogger()
	connectLogger := logger
	if connectLogger != nil {
		connectLogger = connectLogger.With("component", "device")
	}
	s := &DeviceServer{port: options.Port}
	s.setPairingPort(options.RemotePairingPort)
	server, err := deviceserver.New(deviceserver.Options{
		Connect: func(ctx context.Context) (*coredevice.Device, error) {
			port := uint16(s.pairingPort.Load())
			if file.lockdown != nil {
				port = lockdown.Port
			}
			peer, err := vpnpeer.Find(ctx, port)
			if err != nil {
				return nil, err
			}
			s.loopback.Store(newLoopbackAddress(peer))
			if connectLogger != nil {
				connectLogger.Info("device address", "address", peer.Address, "local_network", peer.LocalNetwork)
			}
			if file.lockdown != nil {
				return coredevice.ConnectLockdown(ctx, coredevice.LockdownOptions{
					Address: peer.Address,
					Record:  file.lockdown,
					Logger:  connectLogger,
				})
			}
			return coredevice.ConnectRemotePairing(ctx, coredevice.RemotePairingOptions{
				Address: netip.AddrPortFrom(peer.Address, port),
				Record:  file.remote,
				Logger:  connectLogger,
			})
		},
		Logger:   logger,
		Observer: &deviceObserver{handler: handler},
		Listen:   net.JoinHostPort("::", strconv.Itoa(int(options.Port))),
		RFB: rfb.ServerOptions{
			Name:             name,
			Username:         options.Username,
			Password:         options.Password,
			NoAuthentication: options.NoAuthentication,
			Media: media.ServerStreamConfig{
				VideoPort: uint16(options.Port + 1),
				AudioPort: uint16(options.Port + 2),
			},
		},
		DiskImageDirectory:     options.DiskImageDirectory,
		AutoUnlockPasscode:     options.AutoUnlockPasscode,
		AutoUnlockPasscodeType: deviceserver.PasscodeType(options.AutoUnlockPasscodeType),
		BacklightState:         handler.BacklightState,
	})
	if err != nil {
		return nil, err
	}
	s.server = server
	return s, nil
}

func (s *DeviceServer) Start() error {
	err := s.server.Start()
	if err != nil {
		return err
	}
	servingDevices.access.Lock()
	servingDevices.ports[s] = s.port
	servingDevices.access.Unlock()
	return nil
}

func (s *DeviceServer) LoopbackAddress() *LoopbackAddress {
	return s.loopback.Load()
}

func (s *DeviceServer) SetRemotePairingPort(port int32) {
	if s.setPairingPort(port) {
		s.server.Retry()
	}
}

func (s *DeviceServer) setPairingPort(port int32) bool {
	if port == 0 {
		port = defaultRemotePairingPort
	}
	return s.pairingPort.Swap(uint32(port)) != uint32(port)
}

func (s *DeviceServer) AdjustBrightness(steps int32) error {
	return s.server.AdjustBrightness(int(steps))
}

func (s *DeviceServer) LockScreen() error {
	return s.server.LockScreen()
}

func (s *DeviceServer) WakeScreen() error {
	return s.server.WakeScreen()
}

func (s *DeviceServer) Close() error {
	servingDevices.access.Lock()
	delete(servingDevices.ports, s)
	servingDevices.access.Unlock()
	return s.server.Close()
}

type deviceObserver struct {
	handler DeviceServerHandler
}

func (o *deviceObserver) StateChanged(state deviceserver.State, err error) {
	code, message := deviceError(err)
	o.handler.OnStateChanged(stateCode(state), code, message)
}

func (o *deviceObserver) DeviceConnected(info coredevice.DeviceInfo) {}

func (o *deviceObserver) ViewerConnected(session *rfb.ServerSession) {
	o.handler.OnViewerConnected(int64(session.ID()))
}

func (o *deviceObserver) ViewerDisconnected(session *rfb.ServerSession, err error) {
	o.handler.OnViewerDisconnected(int64(session.ID()))
}

func (o *deviceObserver) StreamingChanged(active bool) {}

func stateCode(state deviceserver.State) int32 {
	switch state {
	case deviceserver.StateConnecting:
		return DeviceServerConnecting
	case deviceserver.StatePreparing:
		return DeviceServerPreparing
	case deviceserver.StateRunning:
		return DeviceServerRunning
	case deviceserver.StateFailed:
		return DeviceServerFailed
	}
	return DeviceServerStopped
}

func deviceError(err error) (int32, string) {
	if err == nil {
		return DeviceErrorNone, ""
	}
	return int32(deviceserver.ProblemOf(err)), err.Error()
}
