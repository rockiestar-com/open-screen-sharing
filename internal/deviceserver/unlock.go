package deviceserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/accessibility"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/hid"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
)

type PasscodeType int

const (
	PasscodePIN PasscodeType = iota
	PasscodeNumeric
	PasscodeAlphanumeric
)

const (
	usageDigit1 = 0x1e
	usageDigit0 = 0x27
)

const maxPasscodeLength = 64

func validateAutoUnlock(options Options) error {
	passcode := options.AutoUnlockPasscode
	if passcode == "" {
		return nil
	}
	if len(passcode) > maxPasscodeLength {
		return E.New("the device passcode must contain at most 64 characters")
	}
	switch options.AutoUnlockPasscodeType {
	case PasscodePIN:
		if len(passcode) != 4 && len(passcode) != 6 {
			return E.New("a fixed-length device PIN must contain 4 or 6 digits")
		}
	case PasscodeNumeric, PasscodeAlphanumeric:
	default:
		return E.New("unknown device passcode type")
	}
	for index := range len(passcode) {
		character := passcode[index]
		if options.AutoUnlockPasscodeType != PasscodeAlphanumeric && (character < '0' || character > '9') {
			return E.New("a numeric device passcode can contain only digits")
		}
		if _, ok := asciiKeystrokes[character]; !ok {
			return E.New("automatic unlocking supports only printable ASCII characters")
		}
	}
	return nil
}

type deviceUnlockTarget struct {
	worker          *inputWorker
	device          *attachment
	accessibility   *accessibility.RemoteClient
	pad             *accessibility.PasscodePad
	touchesPrepared <-chan struct{}
	touchesError    error
}

func (t *deviceUnlockTarget) unlock(ctx context.Context) (consumed bool, err error) {
	options := t.worker.server.options
	locked, err := t.locked(ctx)
	if err != nil || !locked {
		return false, err
	}
	if err = t.prepare(ctx); err != nil {
		return true, err
	}
	clearLength := maxPasscodeLength
	if options.AutoUnlockPasscodeType == PasscodePIN {
		clearLength = 6
	}
	locked, err = t.clear(ctx, clearLength)
	if err != nil || !locked {
		return true, err
	}
	ready, err := t.ready(ctx)
	if err != nil {
		return true, err
	}
	if !ready {
		return true, E.New("the lock-screen passcode field lost keyboard focus")
	}
	strokes := common.Map([]byte(options.AutoUnlockPasscode), func(character byte) asciiKeystroke {
		return asciiKeystrokes[character]
	})
	if options.AutoUnlockPasscodeType != PasscodePIN {
		strokes = append(strokes, asciiKeystroke{usage: usageReturn})
	}
	for _, stroke := range strokes {
		if locked, err = t.locked(ctx); err != nil || !locked {
			return true, err
		}
		if err = t.key(ctx, stroke); err != nil {
			return true, err
		}
	}
	for {
		if locked, err = t.locked(ctx); err != nil || !locked {
			return true, err
		}
	}
}

func (t *deviceUnlockTarget) locked(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	w := t.worker
	for {
		reused := w.lockState != nil
		if !reused {
			conn, err := t.device.device.DialLockState(ctx)
			if err != nil {
				return false, err
			}
			w.lockState = conn
		}
		state, err := w.lockState.LockState(ctx)
		if err == nil {
			return state.PasscodeRequired, nil
		}
		w.lockState.Close()
		w.lockState = nil
		if !reused || ctx.Err() != nil {
			return false, err
		}
	}
}

func (t *deviceUnlockTarget) prepare(ctx context.Context) error {
	started := time.Now()
	w := t.worker
	if err := w.client.ReleaseAll(ctx); err != nil {
		return err
	}
	w.resetState()
	if err := w.keys.capsLockKey(ctx, w.client, false); err != nil {
		return err
	}
	if t.device.device.Info().DeviceClass == "iPad" {
		return t.prepareTouchPad(ctx, started)
	}
	if t.device.device.Info().DeviceClass == "iPhone" {
		touchContext, cancelTouches := context.WithCancel(ctx)
		prepared := make(chan struct{})
		t.touchesPrepared = prepared
		go func() {
			defer close(prepared)
			phaseStarted := time.Now()
			t.touchesError = w.client.PrepareTouches(touchContext)
			w.logger.Info("unlock touch transport initialized", "elapsed", time.Since(phaseStarted), "error", t.touchesError)
		}()
		defer func() {
			cancelTouches()
			<-prepared
		}()
	}
	client, err := accessibility.DialRemote(ctx, t.device.owner, t.device.device)
	if err != nil {
		return E.Cause(err, "read the lock-screen keyboard focus")
	}
	t.accessibility = client
	wakeSent, openSent := false, false
	shortcutTried := false
	for {
		locked, err := t.locked(ctx)
		if err != nil || !locked {
			return err
		}
		active, err := t.activeDisplay(ctx)
		if err != nil {
			return err
		}
		if !active {
			if !wakeSent {
				if locked, err = t.locked(ctx); err != nil || !locked {
					return err
				}
				if err = t.key(ctx, asciiKeystroke{usage: usageBackspace}); err != nil {
					return err
				}
				wakeSent = true
			}
			continue
		}
		state, layout, err := t.preparation(ctx, openSent && !shortcutTried)
		if err != nil {
			return err
		}
		if state.Ready {
			w.logger.Info("lock-screen keyboard ready", "elapsed", time.Since(started))
			return nil
		}
		if layout != nil {
			data, _ := json.Marshal(layout)
			w.logger.Info("unlock layout probe", "elapsed", time.Since(started), "layout", string(data))
			shortcutTried, err = t.requestPasscode(ctx, *layout)
			if err != nil {
				return err
			}
			if shortcutTried {
				continue
			}
		}
		// A wake key and the first key opening the passcode page can be consumed.
		if !openSent {
			if state.Diagnostics.SpringBoard == nil || !*state.Diagnostics.SpringBoard {
				continue
			}
			if locked, err = t.locked(ctx); err != nil || !locked {
				return err
			}
			if err = t.key(ctx, asciiKeystroke{usage: usageBackspace}); err != nil {
				return err
			}
			openSent = true
			w.logger.Info("unlock first backspace", "elapsed", time.Since(started))
		}
	}
}

func (t *deviceUnlockTarget) requestPasscode(ctx context.Context, layout accessibility.PasscodeLayout) (bool, error) {
	deviceClass := t.device.device.Info().DeviceClass
	if _, ok := accessibility.NumericPasscodeShortcutPoint(layout, deviceClass); !ok {
		return false, nil
	}
	phaseStarted := time.Now()
	var err error
	if t.touchesPrepared != nil {
		select {
		case <-t.touchesPrepared:
			err = t.touchesError
		case <-ctx.Done():
			err = ctx.Err()
		}
		t.worker.logger.Info("unlock touch transport joined", "elapsed", time.Since(phaseStarted), "error", err)
	} else {
		err = t.worker.client.PrepareTouches(ctx)
	}
	if err != nil {
		return false, E.Cause(err, "prepare passcode shortcut")
	}
	t.worker.logger.Info("unlock shortcut transport ready", "elapsed", time.Since(phaseStarted))
	phaseStarted = time.Now()
	state, fresh, err := t.preparation(ctx, true)
	t.worker.logger.Info("unlock shortcut recheck", "elapsed", time.Since(phaseStarted), "ready", state.Ready, "error", err)
	if err != nil || state.Ready || fresh == nil {
		return false, err
	}
	layout = *fresh
	point, ok := accessibility.NumericPasscodeShortcutPoint(layout, deviceClass)
	if !ok {
		return false, nil
	}
	phaseStarted = time.Now()
	locked, err := t.locked(ctx)
	t.worker.logger.Info("unlock shortcut lock recheck", "elapsed", time.Since(phaseStarted), "locked", locked, "error", err)
	if err != nil || !locked {
		return false, err
	}
	started := time.Now()
	data, _ := json.Marshal(layout)
	t.worker.logger.Info("unlock shortcut tap", "layout", string(data))
	err = t.worker.client.Tap(ctx, hid.Touch{X: point.X, Y: point.Y})
	t.worker.logger.Info("unlock shortcut tap finished", "elapsed", time.Since(started), "error", err)
	return true, err
}

// SpringBoard counts Home downs that carry no press count itself and takes a
// second down within its multiple-press interval as a double press, which the
// lock screen consumes without showing the unlock UI. The interval is 0.3 s,
// or 0.35 s when the configured value is rejected (iPadOS 27.2 24B5110a,
// SpringBoard 0x226a2ea28, 0x226e5c248, 0x226e16f64).
const homePressSpacing = 500 * time.Millisecond

// The iPad passcode page ignores a hardware keyboard even when its field is
// the first responder. Home wakes the display and then asks for the unlock UI
// (24B5110a SpringBoard); a touch up anywhere on the biometric button skips the
// face scan (24A446 SpringBoardUIServices 0x1a7dada7c, 0x1a7dafa90).
func (t *deviceUnlockTarget) prepareTouchPad(ctx context.Context, started time.Time) error {
	w := t.worker
	if t.worker.server.options.AutoUnlockPasscodeType != PasscodePIN {
		return E.New("automatic unlocking on iPad supports only a fixed-length PIN")
	}
	if err := w.client.PrepareTouches(ctx); err != nil {
		return err
	}
	home := hid.ButtonHome
	home.Hold = 0
	// Home is idempotent while the unlock UI is shown.
	active, err := t.activeDisplay(ctx)
	if err != nil {
		return err
	}
	if locked, err := t.locked(ctx); err != nil || !locked {
		return err
	}
	if err = w.client.PressButton(ctx, home); err != nil {
		return err
	}
	pressed := time.Now()
	wakeOnly := !active
	w.logger.Info("unlock home", "elapsed", time.Since(started), "wake", wakeOnly)
	if t.accessibility, err = accessibility.DialRemote(ctx, t.device.owner, t.device.device); err != nil {
		return E.Cause(err, "read the lock-screen keyboard focus")
	}
	tapped := false
	for {
		locked, err := t.locked(ctx)
		if err != nil || !locked {
			return err
		}
		state, layout, err := t.preparation(ctx, !tapped)
		if err != nil {
			return err
		}
		if state.Ready {
			break
		}
		if tapped {
			continue
		}
		if layout == nil || layout.Keyboard == nil {
			// The keypad exists only once the unlock UI does.
			active, err := t.activeDisplay(ctx)
			if err != nil {
				return err
			}
			if wakeOnly && active {
				if err = sleep(ctx, homePressSpacing-time.Since(pressed)); err != nil {
					return err
				}
				if locked, err = t.locked(ctx); err != nil || !locked {
					return err
				}
				if err = w.client.PressButton(ctx, home); err != nil {
					return err
				}
				wakeOnly = false
				w.logger.Info("unlock home", "elapsed", time.Since(started), "wake", wakeOnly)
			}
			continue
		}
		point, ok := accessibility.NumericPasscodeShortcutPoint(*layout, "iPad")
		if !ok {
			// Touch ID lays its visible button out below the glyph and text.
			if point, ok, err = t.accessibility.FindUnlockButton(ctx, *layout); err != nil {
				return err
			}
			if !ok {
				continue
			}
		}
		if locked, err = t.locked(ctx); err != nil || !locked {
			return err
		}
		err = w.client.Tap(ctx, hid.Touch{X: point.X, Y: point.Y})
		tapped = true
		w.logger.Info("unlock shortcut tap", "elapsed", time.Since(started), "error", err)
		if err != nil {
			return err
		}
	}
	for {
		pad, err := t.accessibility.ReadPasscodePad(ctx)
		if err == nil {
			t.pad = pad
			w.logger.Info("lock-screen keypad ready", "elapsed", time.Since(started))
			return nil
		}
		if !errors.Is(err, accessibility.ErrPasscodePadUnavailable) {
			return err
		}
		if locked, err := t.locked(ctx); err != nil || !locked {
			return err
		}
	}
}

func (t *deviceUnlockTarget) clear(ctx context.Context, length int) (bool, error) {
	for range length {
		locked, err := t.locked(ctx)
		if err != nil || !locked {
			return false, err
		}
		if t.pad != nil {
			entered, err := t.accessibility.PasscodeEntered(ctx, t.pad)
			if err != nil || !entered {
				return err == nil, err
			}
			err = t.worker.client.Tap(ctx, hid.Touch{X: t.pad.Remove.X, Y: t.pad.Remove.Y})
			if err != nil {
				return false, err
			}
			continue
		}
		if err = t.key(ctx, asciiKeystroke{usage: usageBackspace}); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (t *deviceUnlockTarget) ready(ctx context.Context) (bool, error) {
	if t.pad != nil {
		return true, nil
	}
	state, _, err := t.preparation(ctx, false)
	return state.Ready, err
}

func (t *deviceUnlockTarget) preparation(ctx context.Context, readLayout bool) (accessibility.PasscodeState, *accessibility.PasscodeLayout, error) {
	active, err := t.activeDisplay(ctx)
	if err != nil || !active {
		return accessibility.PasscodeState{}, nil, err
	}
	var state accessibility.PasscodeState
	var layout *accessibility.PasscodeLayout
	if readLayout {
		state, layout, err = t.accessibility.ReadPasscodePreparation(ctx)
	} else {
		state, err = t.accessibility.ReadPasscodeState(ctx)
	}
	data, _ := json.Marshal(state)
	t.worker.logger.Info("unlock readiness probe", "state", string(data), "error", err)
	return state, layout, err
}

func (t *deviceUnlockTarget) activeDisplay(ctx context.Context) (bool, error) {
	if read := t.worker.server.options.BacklightState; read != nil {
		if state := read(); state >= 0 {
			return state == 2, nil
		}
	}
	state, err := t.device.display.BacklightState(ctx)
	return state == "activeOn", err
}

func (t *deviceUnlockTarget) key(ctx context.Context, stroke asciiKeystroke) error {
	input := t.worker.client
	if t.pad != nil {
		if stroke.usage < usageDigit1 || stroke.usage > usageDigit0 || stroke.shift {
			return E.New("the iPad keypad has only digit keys")
		}
		point := t.pad.Digits[(stroke.usage-usageDigit1+1)%10]
		return input.Tap(ctx, hid.Touch{X: point.X, Y: point.Y})
	}
	if stroke.shift {
		if err := input.KeyDown(ctx, usageLeftShift); err != nil {
			return err
		}
	}
	if err := input.KeyDown(ctx, stroke.usage); err != nil {
		return err
	}
	// Once the field is ready, immediate down/up transitions, each key after a
	// lock-state read, passed ten prefix-and-clear trials on iPhone18,2 / iOS
	// 27.2 24B5110a.
	if err := input.KeyUp(ctx, stroke.usage); err != nil {
		return err
	}
	if stroke.shift {
		if err := input.KeyUp(ctx, usageLeftShift); err != nil {
			return err
		}
	}
	return nil
}

func (w *inputWorker) checkUnlock(a *attachment) bool {
	if len(w.server.rfb.Sessions()) == 0 {
		w.unlockPending = false
		return false
	}
	buttons := &w.server.buttons.access
	buttons.Lock()
	defer buttons.Unlock()
	started := time.Now()
	target := &deviceUnlockTarget{worker: w, device: a}
	consumed, err := target.unlock(a.ctx)
	if consumed || err != nil {
		released := a.ctx.Err() == nil
		if released {
			if releaseErr := w.client.ReleaseAll(a.ctx); releaseErr != nil {
				w.logger.Warn("release input after automatic unlocking", "error", releaseErr)
				released = false
			}
		}
		if err != nil || !released {
			w.closeClient()
		} else {
			w.resetState()
		}
		w.access.Lock()
		w.queue = slices.DeleteFunc(w.queue, func(item inputItem) bool { return item.event != nil })
		w.access.Unlock()
	}
	if target.accessibility != nil {
		cleanupStarted := time.Now()
		if closeErr := target.accessibility.Close(); closeErr != nil {
			closeErr = E.Cause(closeErr, "stop automatic unlock accessibility")
			w.closeErr = E.Errors(w.closeErr, closeErr)
			w.logger.Warn("automatic unlock accessibility cleanup failed", "error", closeErr)
		} else {
			w.logger.Info("automatic unlock accessibility stopped", "elapsed", time.Since(cleanupStarted))
		}
	}
	if consumed {
		w.logger.Info("automatic unlock finished", "elapsed", time.Since(started), "error", err)
	}
	if err != nil && a.ctx.Err() != nil {
		return true
	}
	w.unlockPending = false
	if err != nil {
		w.logger.Warn("automatic unlocking failed", "error", err)
		return true
	}
	return consumed
}
