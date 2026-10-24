package deviceserver

import (
	"sync"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/hid"
	E "github.com/sagernet/sing/common/exceptions"
)

// dtuhidd drops the events it has not dispatched yet once their connection
// ends, however it ends (observed on iPhone18,2, iOS 27.2 24B5110a).
type buttonPresser struct {
	access sync.Mutex
	device *coredevice.Device
	client *hid.Client
}

func (s *Server) AdjustBrightness(steps int) error {
	button := hid.ButtonBrightnessUp
	if steps < 0 {
		button, steps = hid.ButtonBrightnessDown, -steps
	}
	return s.pressButton(button, steps)
}

func (s *Server) LockScreen() error {
	button := hid.ButtonLock
	button.Hold = 0
	return s.pressButton(button, 1)
}

// Home wakes an already-locked display without toggling an ongoing video wake
// back off, as a second power-button event would.
func (s *Server) WakeScreen() error {
	button := hid.ButtonHome
	// Back-to-back down/up woke ActiveOn from AOD on iPhone18,2 / 24B5110a.
	button.Hold = 0
	return s.pressButton(button, 1)
}

func (s *Server) pressButton(button hid.Button, count int) error {
	a := s.currentDevice()
	if a == nil {
		return E.New("the device is not connected")
	}
	b := &s.buttons
	b.access.Lock()
	defer b.access.Unlock()
	s.access.Lock()
	current := !s.closed && s.attached == a && a.ctx.Err() == nil
	s.access.Unlock()
	if !current {
		return E.New("the device is not connected")
	}
	if b.client != nil && b.device != a.device {
		if err := b.closeLocked(); err != nil {
			return E.Cause(err, "release the previous device's buttons")
		}
	}
	if b.client == nil {
		b.client = hid.New(a.owner, a.device, s.logger.With("component", "buttons"))
		b.device = a.device
	}
	var err error
	for range count {
		if err = b.client.PressButton(a.ctx, button); err != nil {
			break
		}
	}
	return err
}

func (b *buttonPresser) close() error {
	b.access.Lock()
	defer b.access.Unlock()
	if b.client == nil {
		return nil
	}
	return b.closeLocked()
}

func (b *buttonPresser) closeLocked() error {
	err := b.client.Close()
	b.client, b.device = nil, nil
	return err
}
