package deviceserver

import (
	"time"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/accessibility"
	E "github.com/sagernet/sing/common/exceptions"
)

func (s *Server) dismissScreenViewingNotifications(a *attachment) error {
	for {
		client, err := accessibility.DialRemote(a.ctx, a.owner, a.device)
		var closeErr error
		if err == nil {
			s.logger.Debug("watching screen-viewing notifications")
			for err == nil {
				var count int
				count, err = client.DismissScreenViewingNotifications(a.ctx)
				if count != 0 {
					s.logger.Debug("cleared screen-viewing notifications", "count", count)
				}
			}
			closeErr = client.Close()
		}
		if a.ctx.Err() != nil {
			return closeErr
		}
		s.logger.Warn("clear screen-viewing notifications", "error", E.Append(err, closeErr, func(err error) error {
			return E.Cause(err, "close remote AX")
		}))
		select {
		case <-a.ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
		}
	}
}
