// dtuhidd serves the RemoteXPC services com.apple.coredevice.hid.indigo,
// whose features include remote.hid.button and remote.hid.digitizer, and
// com.apple.coredevice.hid.universalhidservice, whose feature is
// remote.universalhidservice (DDI 27A9266e,
// /Library/LaunchDaemons/com.apple.coredevice.dtuhidd.plist).
package hid

import (
	"context"
	"log/slog"
	"time"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/xpc"
	E "github.com/sagernet/sing/common/exceptions"
)

const (
	universalService = "com.apple.coredevice.hid.universalhidservice"
	indigoService    = "com.apple.coredevice.hid.indigo"

	universalFeature = "com.apple.coredevice.feature.remote.universalhidservice"
	buttonFeature    = "com.apple.coredevice.feature.remote.hid.button"
	digitizerFeature = "com.apple.coredevice.feature.remote.hid.digitizer"
)

// idevice waits 300 ms after starting the display stream before sending
// HID input (idevice d32c818, tools/src/hid.rs:429-449).
const StreamWarmup = 300 * time.Millisecond

type Touch struct {
	X, Y float64
}

// idevice maps IndigoDigitizerEvent edges None/Top/Left/Bottom/Right to
// 0/1/2/3/4 (idevice d32c818, idevice/src/services/core_device/hid.rs:56-73, 624).
type Edge uint64

const (
	EdgeNone   Edge = 0
	EdgeTop    Edge = 1
	EdgeLeft   Edge = 2
	EdgeBottom Edge = 3
	EdgeRight  Edge = 4
)

// idevice maps IndigoDigitizerEvent Start/Position/End to 0/1/2 (idevice
// d32c818, idevice/src/services/core_device/hid.rs:39-52, 623).
const (
	digitizerStart    = 0
	digitizerPosition = 1
	digitizerEnd      = 2
)

type Client struct {
	ctx    context.Context
	device *coredevice.Device
	logger *slog.Logger

	universal *xpc.Conn
	indigo    *xpc.Conn
	touches   []Touch
	edge      Edge
	keyboard  uint64
	pressed   [keyboardBitmapSize]byte
	buttons   map[Button]struct{}
}

func New(ctx context.Context, device *coredevice.Device, logger *slog.Logger) *Client {
	return &Client{ctx: ctx, device: device, logger: logger}
}

func (c *Client) PrepareTouches(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.indigo != nil {
		return nil
	}
	conn, err := c.device.DialXPC(ctx, indigoService)
	if err != nil {
		return E.Cause(err, "hid")
	}
	c.indigo = conn
	return nil
}

// Zero-hold taps reached the passcode page on iPhone18,2 / iOS 27.2 24B5110a.
func (c *Client) Tap(ctx context.Context, touch Touch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.edge = EdgeNone
	touches := []Touch{touch}
	err := c.sendTouches(ctx, digitizerStart, touches)
	var releaseErr error
	if err == nil {
		releaseErr = c.sendTouches(c.ctx, digitizerEnd, touches)
	}
	return E.Errors(err, ctx.Err(), releaseErr)
}

func (c *Client) SetTouches(ctx context.Context, touches []Touch, edge Edge) error {
	eventType := uint64(digitizerPosition)
	if len(c.touches) == 0 {
		eventType = digitizerStart
		c.edge = edge
	}
	return c.sendTouches(ctx, eventType, touches)
}

func (c *Client) LiftTouches(ctx context.Context, touches []Touch) error {
	if len(c.touches) == 0 {
		return nil
	}
	if len(touches) == 0 {
		touches = c.touches
	}
	return c.sendTouches(ctx, digitizerEnd, touches)
}

// idevice sends at most pointOne and pointTwo in an IndigoDigitizerEvent
// (idevice d32c818, idevice/src/services/core_device/hid.rs:601-632).
func (c *Client) sendTouches(ctx context.Context, eventType uint64, touches []Touch) error {
	payload := map[string]any{
		"pointOne":  digitizerPoint(touches[0]),
		"eventType": eventType,
		"edge":      uint64(c.edge),
		"target":    uint64(0),
	}
	if len(touches) > 1 {
		payload["pointTwo"] = digitizerPoint(touches[1])
	}
	if err := c.sendIndigo(ctx, "IndigoDigitizerEvent", digitizerFeature, payload); err != nil {
		return err
	}
	if eventType == digitizerEnd {
		c.touches = nil
	} else {
		c.touches = append(c.touches[:0], touches...)
	}
	return nil
}

func digitizerPoint(touch Touch) map[string]any {
	return map[string]any{"x": touch.X, "y": touch.Y}
}

// idevice sends Indigo events without a reply flag (idevice d32c818,
// idevice/src/services/core_device/hid.rs:524-541).
func (c *Client) sendIndigo(ctx context.Context, messageType, feature string, payload map[string]any) error {
	if err := c.PrepareTouches(ctx); err != nil {
		return err
	}
	err := c.indigo.SendContext(ctx, map[string]any{
		"messageType":       messageType,
		"featureIdentifier": feature,
		"payload":           payload,
	}, false)
	if err != nil {
		c.closeIndigo()
		return E.Cause(err, "hid: send ", messageType)
	}
	return nil
}

func (c *Client) ReleaseAll(ctx context.Context) error {
	return E.Errors(c.LiftTouches(ctx, nil), c.releaseKeys(ctx), c.releaseButtons(ctx))
}

func (c *Client) Close() error {
	var err error
	if c.ctx.Err() == nil {
		err = E.Errors(c.ReleaseAll(c.ctx), c.removeKeyboard(c.ctx))
	}
	c.closeUniversal()
	c.closeIndigo()
	return err
}

// IndigoHIDSession ends its touchscreen contact then cancels its service pool
// on peer teardown. Raw button events do not record held state there; their
// explicit Up must be sent while the owning connection is alive (DDI 27B5028f,
// dtuhidd 0x10001b09c, 0x100017bfc, 0x100027914, 0x10003023c; button events
// 0x100018714, 0x1000281c0).
func (c *Client) closeIndigo() {
	if c.indigo != nil {
		c.indigo.Close()
		c.indigo = nil
	}
	c.touches = nil
	c.edge = EdgeNone
	c.buttons = nil
}

func (c *Client) sendReport(ctx context.Context, serviceID uint64, report []byte) error {
	conn, err := c.universalConn(ctx)
	if err != nil {
		return err
	}
	err = conn.SendContext(ctx, universalRequest(map[string]any{
		"send": map[string]any{"_0": report, "_1": serviceID},
	}), false)
	if err != nil {
		c.closeUniversal()
		return E.Cause(err, "hid: send report")
	}
	return nil
}

// The UniversalHID request envelope matches idevice (idevice d32c818,
// idevice/src/services/core_device/hid.rs:259-267).
func universalRequest(payload map[string]any) map[string]any {
	return map[string]any{
		"featureIdentifier": universalFeature,
		"messageType":       "Request",
		"payload":           payload,
	}
}

func (c *Client) universalConn(ctx context.Context) (*xpc.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.universal == nil {
		conn, err := c.device.DialXPC(ctx, universalService)
		if err != nil {
			return nil, E.Cause(err, "hid")
		}
		c.universal = conn
	}
	return c.universal, nil
}

// dtuhidd keeps the services created on a universalhidservice connection in a
// pool of that connection, and when the connection closes it resets their
// gestures and cancels them (dtuhidd 651.13.2, DDI 27A9266e, UUID
// C35A167F-1D19-3B5F-AAB9-687431EFC792, sub_10002fd74 0x10002fe7c..0x10002fe98,
// sub_1000129f4 0x1000129f4..0x100012a18, sub_100027f9c 0x1000281ac..0x100028250,
// sub_10003131c 0x10003131c..0x100031780).
func (c *Client) closeUniversal() {
	if c.universal == nil {
		return
	}
	c.universal.Close()
	c.universal = nil
	c.keyboard = 0
	c.pressed = [keyboardBitmapSize]byte{}
}

func uintValue(value any) (uint64, bool) {
	switch value := value.(type) {
	case uint64:
		return value, true
	case int64:
		return uint64(value), value >= 0
	}
	return 0, false
}
