package deviceserver

import (
	"context"

	"github.com/nekohasekai/open-screen-sharing/internal/rfb"
	"github.com/nekohasekai/open-screen-sharing/internal/rfb/media"
)

type viewerHandler Server

func (h *viewerHandler) server() *Server {
	return (*Server)(h)
}

func (h *viewerHandler) Screen() rfb.ServerScreen {
	s := h.server()
	s.access.Lock()
	defer s.access.Unlock()
	return s.screen
}

func (h *viewerHandler) SessionStarted(session *rfb.ServerSession) {
	h.server().notify(func(observer Observer) { observer.ViewerConnected(session) })
	h.server().input.viewerConnected(session)
}

func (h *viewerHandler) SessionEnded(session *rfb.ServerSession, err error) {
	s := h.server()
	s.input.sessionEnded(session)
	s.clip.sharing(session, false)
	s.notify(func(observer Observer) { observer.ViewerDisconnected(session, err) })
}

func (h *viewerHandler) HandleEvent(session *rfb.ServerSession, event rfb.ViewerEvent) {
	s := h.server()
	switch event := event.(type) {
	case rfb.ViewerPointer, rfb.ViewerKey, rfb.ViewerScroll, rfb.ViewerGesture, rfb.ViewerSystemCommand:
		s.input.post(session, event)
	case rfb.ViewerMode:
		if event.Mode == rfb.ModeObserve {
			s.input.sessionEnded(session)
		} else {
			s.input.sessionResumed(session)
		}
	case rfb.ViewerClipboard:
		s.clip.viewerClipboard(event.Clipboard)
	case rfb.ViewerClipboardSharing:
		s.clip.sharing(session, event.Enabled)
	}
}

func (h *viewerHandler) Clipboard(ctx context.Context) (*rfb.Clipboard, error) {
	return h.server().clip.read(ctx)
}

func (h *viewerHandler) OpenVideo(ctx context.Context, request media.ServerVideoRequest) (media.ServerVideoSource, error) {
	return h.server().openVideo(ctx, request)
}

func (h *viewerHandler) OpenAudio() (media.ServerAudioSource, error) {
	return h.server().openAudio()
}
