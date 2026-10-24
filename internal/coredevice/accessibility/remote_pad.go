package accessibility

import (
	"context"

	E "github.com/sagernet/sing/common/exceptions"
)

var ErrPasscodePadUnavailable = E.New("accessibility: the lock-screen keypad is unavailable")

type PasscodePad struct {
	Digits [10]LayoutPoint
	Remove LayoutPoint
	remove translationObject
}

// The keypad exists, hidden, behind the biometric prompt. Its keys are 1-9, 0,
// then Cancel or Delete.
func (c *RemoteClient) ReadPasscodePad(ctx context.Context) (*PasscodePad, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var state PasscodeState
	app, values, _, err := c.readPasscodeApplication(ctx, &state, true)
	if err != nil {
		return nil, err
	}
	if values == nil {
		return nil, E.Extend(ErrPasscodePadUnavailable, state.Reason)
	}
	screen := decodeLayoutFrame(values[uint64(attributeFrame)])
	if screen == nil {
		return nil, E.New("accessibility: the lock-screen frame is unavailable")
	}
	width, height := min(screen.Width, screen.Height), max(screen.Width, screen.Height)
	keyboard, err := c.query(ctx, requestAttribute, &app, attributeNative, 0, map[string]any{"parameter": int64(nativeActiveKeyboard)})
	if err != nil {
		return nil, err
	}
	pad, ok := parseTranslation(keyboard.value)
	if keyboard.code != 0 || !ok || pad.application || pad.pid != app.pid {
		return nil, ErrPasscodePadUnavailable
	}
	class, err := c.query(ctx, requestAttribute, &pad, attributeNative, 0, map[string]any{"parameter": int64(nativeClassName)})
	if err != nil {
		return nil, err
	}
	if class.code != 0 || layoutClassName(class.value) != "SBUIPasscodeLockNumberPad" {
		return nil, E.Extend(ErrPasscodePadUnavailable, "the keyboard is not the numeric keypad")
	}
	children, err := c.query(ctx, requestAttributes, &pad, 0, 0, map[string]any{"attributes": []any{int64(attributeChildren)}})
	if err != nil {
		return nil, err
	}
	var keys []any
	if values, ok := children.value.(map[any]any); ok {
		keys, _ = values[uint64(attributeChildren)].([]any)
	}
	if children.code != 0 || len(keys) != 11 {
		return nil, E.New("accessibility: the lock-screen keypad does not have 11 keys")
	}
	result := &PasscodePad{}
	for index, key := range keys {
		object, ok := parseTranslation(key)
		if !ok || object.application || object.pid != app.pid {
			return nil, E.New("accessibility: the lock-screen keypad key is invalid")
		}
		attributes, err := c.query(ctx, requestAttributes, &object, 0, 0, map[string]any{"attributes": []any{int64(attributeFrame), int64(attributeLabel)}})
		if err != nil {
			return nil, err
		}
		values, _ := attributes.value.(map[any]any)
		if attributes.code != 0 || values == nil {
			return nil, E.New("accessibility: the lock-screen keypad key is unavailable")
		}
		digit := string(rune('1' + index))
		if index == 9 {
			digit = "0"
		}
		if label, _ := values[uint64(attributeLabel)].(string); index < 10 && len(label) == 1 && label[0] >= '0' && label[0] <= '9' && label != digit {
			return nil, E.New("accessibility: the lock-screen keypad is not in the expected order")
		}
		frame := decodeLayoutFrame(values[uint64(attributeFrame)])
		if frame == nil || frame.Width == 0 || frame.Height == 0 {
			return nil, E.New("accessibility: the lock-screen keypad key has no frame")
		}
		center := LayoutPoint{X: (frame.X + frame.Width/2) / width, Y: (frame.Y + frame.Height/2) / height}
		if center.X <= 0 || center.X >= 1 || center.Y <= 0 || center.Y >= 1 {
			return nil, E.New("accessibility: the lock-screen keypad key is outside the screen")
		}
		if index == 10 {
			result.Remove, result.remove = center, object
			break
		}
		result.Digits[(index+1)%10] = center
	}
	return result, nil
}

// The passcode field's accessibility value is a localized string, not the
// entered digits (24A446). The key below zero is Delete while digits are
// entered and Cancel otherwise; only Delete carries these traits.
func (c *RemoteClient) PasscodeEntered(ctx context.Context, pad *PasscodePad) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	response, err := c.query(ctx, requestAttributes, &pad.remove, 0, 0, map[string]any{"attributes": []any{int64(attributeTraits)}})
	if err != nil {
		return false, err
	}
	values, _ := response.value.(map[any]any)
	traits, ok := unsigned(values[uint64(attributeTraits)])
	if response.code != 0 || !ok {
		return false, E.New("accessibility: the lock-screen delete key is unavailable")
	}
	return traits&(traitPlaysSound|traitKeyboardKey) != 0, nil
}
