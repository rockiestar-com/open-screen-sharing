package deviceserver

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/hid"
	"github.com/nekohasekai/open-screen-sharing/internal/rfb"
	E "github.com/sagernet/sing/common/exceptions"
)

const inputQueueLimit = 4096

type inputItem struct {
	session   *rfb.ServerSession
	owner     context.Context
	event     rfb.ViewerEvent
	write     *rfb.Clipboard
	release   bool
	moves     bool
	unlock    bool
	forgotten chan error
}

type inputSession struct {
	context context.Context
	cancel  context.CancelFunc
}

type inputWorker struct {
	server *Server
	logger *slog.Logger
	done   chan struct{}

	access   sync.Mutex
	queue    []inputItem
	dropping bool
	buttons  map[*rfb.ServerSession]uint8
	sessions map[*rfb.ServerSession]inputSession
	wake     chan struct{}

	device        *coredevice.Device
	client        *hid.Client
	generation    uint64
	touch         touchMapper
	keys          *keyboard
	unlockPending bool
	lockState     *coredevice.LockStateConn
	closeErr      error
}

func newInputWorker(server *Server, logger *slog.Logger) *inputWorker {
	return &inputWorker{
		server:   server,
		logger:   logger,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		keys:     newKeyboard(),
		buttons:  make(map[*rfb.ServerSession]uint8),
		sessions: make(map[*rfb.ServerSession]inputSession),
	}
}

func (w *inputWorker) push(item inputItem) {
	w.access.Lock()
	if len(w.queue) >= inputQueueLimit && item.event != nil {
		if !w.dropping {
			w.logger.Warn("the device takes input slower than the viewer sends it; dropping input")
		}
		w.dropping = true
		w.access.Unlock()
		return
	}
	w.dropping = false
	w.queue = append(w.queue, item)
	w.access.Unlock()
	w.signal()
}

func (w *inputWorker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *inputWorker) post(session *rfb.ServerSession, event rfb.ViewerEvent) {
	w.access.Lock()
	owner, present := w.sessions[session]
	if !present || owner.context.Err() != nil {
		w.access.Unlock()
		return
	}
	pointer, isPointer := event.(rfb.ViewerPointer)
	if !isPointer {
		w.access.Unlock()
		w.push(inputItem{session: session, owner: owner.context, event: event})
		return
	}
	held, known := w.buttons[session]
	w.buttons[session] = pointer.Buttons
	moves := known && held == pointer.Buttons
	if count := len(w.queue); moves && count > 0 {
		last := &w.queue[count-1]
		if last.moves && last.session == session && last.owner == owner.context {
			last.event = pointer
			w.access.Unlock()
			return
		}
	}
	w.access.Unlock()
	w.push(inputItem{session: session, owner: owner.context, event: event, moves: moves})
}

func (w *inputWorker) postClipboard(clipboard *rfb.Clipboard) {
	w.push(inputItem{write: clipboard})
}

func (w *inputWorker) sessionEnded(session *rfb.ServerSession) {
	w.access.Lock()
	if owner, present := w.sessions[session]; present {
		owner.cancel()
		delete(w.sessions, session)
	}
	delete(w.buttons, session)
	w.access.Unlock()
	w.push(inputItem{session: session, release: true})
}

func (w *inputWorker) forgetAndWait() error {
	forgotten := make(chan error, 1)
	w.push(inputItem{forgotten: forgotten})
	select {
	case err := <-forgotten:
		return err
	case <-w.done:
		return w.closeErr
	}
}

func (w *inputWorker) viewerConnected(session *rfb.ServerSession) {
	w.access.Lock()
	owner, cancel := context.WithCancel(session.Context())
	w.sessions[session] = inputSession{context: owner, cancel: cancel}
	w.access.Unlock()
	if w.server.options.AutoUnlockPasscode != "" {
		w.push(inputItem{unlock: true})
	}
}

func (w *inputWorker) sessionResumed(session *rfb.ServerSession) {
	w.access.Lock()
	defer w.access.Unlock()
	if _, present := w.sessions[session]; !present {
		owner, cancel := context.WithCancel(session.Context())
		w.sessions[session] = inputSession{context: owner, cancel: cancel}
	}
}

func (w *inputWorker) videoReady() {
	if w.server.options.AutoUnlockPasscode != "" {
		w.push(inputItem{})
	}
}

func (w *inputWorker) run(ctx context.Context) {
	defer close(w.done)
	defer w.closeClient()
	for {
		item, ok := w.next(ctx)
		if !ok {
			return
		}
		w.handle(item)
	}
}

func (w *inputWorker) next(ctx context.Context) (inputItem, bool) {
	for {
		w.access.Lock()
		if len(w.queue) > 0 {
			item := w.queue[0]
			w.queue[0] = inputItem{}
			w.queue = w.queue[1:]
			w.access.Unlock()
			return item, true
		}
		w.access.Unlock()
		select {
		case <-ctx.Done():
			return inputItem{}, false
		case <-w.wake:
		}
	}
}

func (w *inputWorker) closeClient() {
	var err error
	if w.client != nil {
		err = w.client.Close()
	}
	if w.lockState != nil {
		err = E.Append(err, w.lockState.Close(), func(err error) error {
			return E.Cause(err, "close lock-state connection")
		})
	}
	w.client = nil
	w.lockState = nil
	w.device = nil
	w.resetState()
	w.keys.capsLock = false
	if err != nil {
		w.logger.Warn("close device input", "error", err)
		w.closeErr = E.Append(w.closeErr, err, func(err error) error {
			return E.Cause(err, "close device input")
		})
	}
}

func (w *inputWorker) resetState() {
	w.touch.reset()
	w.keys.reset()
}

func (w *inputWorker) handle(item inputItem) {
	if item.forgotten != nil {
		w.closeClient()
		item.forgotten <- w.closeErr
		w.closeErr = nil
		return
	}
	if item.unlock {
		w.unlockPending = true
	}
	a := w.server.currentDevice()
	if w.client != nil && (a == nil || a.device != w.device) {
		w.closeClient()
	}
	if a == nil {
		return
	}
	switch {
	case item.write != nil:
		w.writeClipboard(a, item.write)
		return
	case item.release:
		if w.client != nil {
			if len(w.server.rfb.Sessions()) == 0 {
				w.closeClient()
				return
			}
			if err := w.client.ReleaseAll(a.ctx); err != nil {
				w.logger.Warn("release input", "error", err)
				w.closeClient()
			} else {
				w.resetState()
			}
		}
		return
	case item.event == nil && !w.unlockPending:
		return
	}
	running, _, generation := w.server.streams.inputState()
	if !running || a.ctx.Err() != nil {
		return
	}
	if w.client == nil {
		w.client = hid.New(a.owner, a.device, w.logger)
		w.device = a.device
		w.generation = generation
	}
	if w.unlockPending && w.checkUnlock(a) {
		return
	}
	if item.event == nil || item.owner.Err() != nil {
		return
	}
	eventContext, cancel := inputContext(a, item.owner)
	defer cancel()
	if generation != w.generation {
		w.generation = generation
		if err := w.client.ReleaseAll(eventContext); err != nil {
			w.logger.Warn("release input after the video restarted", "error", err)
			w.closeClient()
			return
		}
		w.resetState()
	}
	running, wait, currentGeneration := w.server.streams.inputState()
	if !running || currentGeneration != generation {
		return
	}
	if sleep(eventContext, wait) != nil {
		return
	}
	running, _, currentGeneration = w.server.streams.inputState()
	if !running || currentGeneration != generation || eventContext.Err() != nil {
		return
	}
	w.touch.input = w.client
	w.touch.geometry = w.server.geometry()
	if w.touch.geometry.Width < 2 || w.touch.geometry.Height < 2 {
		return
	}
	if err := w.dispatch(eventContext, item); err != nil {
		w.logger.Debug("send input", "event", item.event, "error", err)
		w.closeClient()
	}
}

func inputContext(a *attachment, viewer context.Context) (context.Context, context.CancelFunc) {
	operation, cancel := context.WithCancel(a.ctx)
	stop := context.AfterFunc(viewer, cancel)
	if viewer.Err() != nil {
		cancel()
	}
	return operation, func() {
		stop()
		cancel()
	}
}

func (w *inputWorker) dispatch(ctx context.Context, item inputItem) error {
	switch event := item.event.(type) {
	case rfb.ViewerPointer:
		return w.touch.pointer(ctx, event)
	case rfb.ViewerKey:
		return w.keys.handle(ctx, w.client, event)
	case rfb.ViewerScroll:
		if event.Momentum != 0 {
			return nil
		}
		if event.Continuous && event.Phase != rfb.PhaseNone {
			return w.touch.continuousScroll(ctx, event)
		}
		deltaX, deltaY := w.mergeWheel(ctx, item.session, event)
		return w.touch.wheel(ctx, event.X, event.Y, deltaX, deltaY)
	case rfb.ViewerGesture:
		if event.Kind == rfb.GestureKindScroll {
			return nil
		}
		return w.touch.gesture(ctx, event)
	case rfb.ViewerSystemCommand:
		return w.touch.systemCommand(ctx, event.Command)
	}
	return nil
}

func isWheel(event rfb.ViewerScroll) bool {
	return event.Momentum == 0 && (!event.Continuous || event.Phase == rfb.PhaseNone)
}

func wheelDelta(event rfb.ViewerScroll) (float64, float64) {
	deltaX, deltaY := event.DeltaX, event.DeltaY
	if deltaX == 0 && deltaY == 0 {
		deltaX, deltaY = float64(event.LinesX*10), float64(event.LinesY*10)
	}
	return deltaX, deltaY
}

func (w *inputWorker) mergeWheel(ctx context.Context, session *rfb.ServerSession, first rfb.ViewerScroll) (float64, float64) {
	deltaX, deltaY := wheelDelta(first)
	if sleep(ctx, wheelMerge) != nil {
		return deltaX, deltaY
	}
	w.access.Lock()
	defer w.access.Unlock()
	for len(w.queue) > 0 {
		next := w.queue[0]
		scroll, isScroll := next.event.(rfb.ViewerScroll)
		if !isScroll || next.session != session || !isWheel(scroll) {
			break
		}
		x, y := wheelDelta(scroll)
		deltaX += x
		deltaY += y
		w.queue[0] = inputItem{}
		w.queue = w.queue[1:]
	}
	return deltaX, deltaY
}
