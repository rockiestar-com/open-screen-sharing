//go:build darwin && !ios

package main

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/imagemounter"
	"github.com/nekohasekai/open-screen-sharing/internal/deviceserver"
	"github.com/nekohasekai/open-screen-sharing/internal/rfb"
	"github.com/nekohasekai/open-screen-sharing/internal/rfb/media"
	E "github.com/sagernet/sing/common/exceptions"

	"github.com/spf13/cobra"
)

const (
	passwordEnvironment       = "IOS_SERVER_PASSWORD"
	unlockPasscodeEnvironment = "IOS_SERVER_UNLOCK_PASSCODE"
)

var (
	serveDevice             string
	servePort               uint16
	serveUsername           string
	servePassword           string
	serveUnlockPasscode     string
	serveUnlockPasscodeType string
)

var commandServe = &cobra.Command{
	Use:   "serve",
	Short: "Share the device's screen with Screen Sharing viewers",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		err := serve(cmd.Context())
		if err != nil {
			fatal(err)
		}
	},
}

func init() {
	flags := commandServe.Flags()
	flags.StringVarP(&serveDevice, "device", "d", "", "UDID of the device to share, needed when more than one is connected")
	flags.Uint16VarP(&servePort, "port", "p", 5910, "port viewers connect to; video and audio use the next two ports")
	flags.StringVar(&serveUsername, "username", "", "user name viewers sign in with")
	flags.StringVar(&servePassword, "password", "", "password viewers sign in with, or set "+passwordEnvironment+"; without a user name and password, viewers connect without signing in")
	flags.StringVar(&serveUnlockPasscode, "unlock-passcode", "", "the device's passcode, to unlock it automatically, or set "+unlockPasscodeEnvironment)
	flags.StringVar(&serveUnlockPasscodeType, "unlock-passcode-type", "pin", "type of the device's passcode: pin (4 or 6 digits), numeric or alphanumeric")
	mainCommand.AddCommand(commandServe)
}

func serve(ctx context.Context) error {
	if servePort == 0 || servePort > 65533 {
		return E.New("--port must be from 1 to 65533, since video and audio use the next two ports")
	}
	password := servePassword
	if password == "" {
		password = os.Getenv(passwordEnvironment)
	}
	if (serveUsername == "") != (password == "") {
		return E.New("--username and --password must be set together")
	}
	unlockPasscode := serveUnlockPasscode
	if unlockPasscode == "" {
		unlockPasscode = os.Getenv(unlockPasscodeEnvironment)
	}
	var passcodeType deviceserver.PasscodeType
	switch serveUnlockPasscodeType {
	case "pin":
		passcodeType = deviceserver.PasscodePIN
	case "numeric":
		passcodeType = deviceserver.PasscodeNumeric
	case "alphanumeric":
		passcodeType = deviceserver.PasscodeAlphanumeric
	default:
		return E.New("--unlock-passcode-type must be pin, numeric or alphanumeric")
	}
	device, err := selectDevice(ctx, serveDevice)
	if err != nil {
		return err
	}
	name := device.Name
	if name == "" {
		name = "iPhone"
	}
	deviceLogger := logger.With("component", "device")
	observer := &serveObserver{failed: make(chan error, 1)}
	server, err := deviceserver.New(deviceserver.Options{
		Connect: func(ctx context.Context) (*coredevice.Device, error) {
			return coredevice.ConnectUSBMux(ctx, device.UDID, deviceLogger)
		},
		Logger:   logger,
		Observer: observer,
		Listen:   net.JoinHostPort("::", strconv.Itoa(int(servePort))),
		RFB: rfb.ServerOptions{
			Name:             name,
			Username:         serveUsername,
			Password:         password,
			NoAuthentication: password == "",
			Media: media.ServerStreamConfig{
				AllowLoopbackTarget: true,
				VideoPort:           servePort + 1,
				AudioPort:           servePort + 2,
			},
		},
		AutoUnlockPasscode:     unlockPasscode,
		AutoUnlockPasscodeType: passcodeType,
		DiskImageDirectory:     imagemounter.XcodeDirectory,
	})
	if err != nil {
		return err
	}
	logger.Info("sharing the device's screen", "name", name, "udid", device.UDID, "port", servePort)
	err = server.Start()
	if err != nil {
		return err
	}
	var failure error
	select {
	case <-ctx.Done():
	case failure = <-observer.failed:
	}
	server.Close()
	if deviceserver.ProblemOf(failure) == deviceserver.ProblemNoDiskImage {
		_, err = os.Stat(filepath.Join(imagemounter.XcodeDirectory, "BuildManifest.plist"))
		if errors.Is(err, fs.ErrNotExist) {
			return E.New("the device has no Developer Disk Image mounted, and this Mac has none in ", imagemounter.XcodeDirectory, "; install Xcode and open it once")
		}
	}
	return failure
}

type serveObserver struct {
	failed chan error
}

func (o *serveObserver) StateChanged(state deviceserver.State, err error) {
	if state == deviceserver.StateFailed {
		select {
		case o.failed <- err:
		default:
		}
	}
}

func (o *serveObserver) DeviceConnected(info coredevice.DeviceInfo) {}

func (o *serveObserver) ViewerConnected(session *rfb.ServerSession) {}

func (o *serveObserver) ViewerDisconnected(session *rfb.ServerSession, err error) {}

func (o *serveObserver) StreamingChanged(active bool) {}
