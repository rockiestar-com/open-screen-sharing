package deviceserver

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/nekohasekai/open-screen-sharing/internal/coredevice"
	"github.com/nekohasekai/open-screen-sharing/internal/coredevice/imagemounter"
	E "github.com/sagernet/sing/common/exceptions"
)

var errRemounted = E.New("the Developer Disk Image is mounted now; reconnecting to reach its services")

// The Developer Disk Image has a Cryptex identity without ApBoardID or ApChipID; its personalized identities include both
// (DDI 27A9266e, BuildManifest.plist, BuildIdentities 0..139 and 140).
var diskImageVariants = []imagemounter.Variant{imagemounter.Cryptex, imagemounter.Personalized}

func (s *Server) prepareDiskImage(ctx context.Context, device *coredevice.Device, images *imagemounter.Client) error {
	if device.HasService(displayService) {
		s.relisted = false
		return nil
	}
	status, err := images.Status(ctx)
	if err != nil {
		return E.Cause(err, "read the Developer Disk Image's state")
	}
	if status.Mounted() {
		if !s.relisted {
			s.relisted = true
			return errRemounted
		}
		return nil
	}
	if !status.DeveloperMode {
		return newError(ProblemDeveloperModeOff, "Developer Mode is off; turn it on in Settings › Privacy & Security › Developer Mode")
	}
	directory := s.options.DiskImageDirectory
	if directory == "" {
		return newError(ProblemNoDiskImage, "the Developer Disk Image is not mounted, and there is no directory for its files")
	}
	var failure error
	variants := diskImageVariants
	if status.CryptexError != nil {
		s.logger.Warn("cryptexd failed; trying the personalized disk image first", "error", status.CryptexError)
		failure = E.Cause(status.CryptexError, "list installed cryptexes")
		variants = []imagemounter.Variant{imagemounter.Personalized, imagemounter.Cryptex}
	}
	found, hasCopy := false, false
	for _, variant := range variants {
		files, err := s.diskImageFiles(ctx, directory, variant)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.Info("disk image files", "variant", variant, "error", err)
			failure = E.Errors(failure, err)
			hasCopy = hasCopy || !errors.Is(err, fs.ErrNotExist)
			continue
		}
		found = true
		s.logger.Info("mounting the Developer Disk Image", "variant", variant, "directory", files)
		if variant == imagemounter.Cryptex {
			err = images.MountCryptex(ctx, files)
		} else {
			err = images.MountPersonalized(ctx, files)
		}
		switch {
		case err == nil, errors.Is(err, imagemounter.ErrAlreadyMounted):
			s.logger.Info("mounted the Developer Disk Image", "variant", variant)
			s.relisted = true
			return errRemounted
		case errors.Is(err, imagemounter.ErrDeveloperModeOff):
			return &Error{Problem: ProblemDeveloperModeOff, Err: err}
		case ctx.Err() != nil:
			return ctx.Err()
		}
		s.logger.Warn("mount the Developer Disk Image", "variant", variant, "error", err)
		failure = E.Errors(failure, E.Cause(err, "mount ", variant))
	}
	if !found {
		if !hasCopy {
			return newError(ProblemNoDiskImage, "the Developer Disk Image is not mounted, and there is no copy of it to mount")
		}
		return &Error{Problem: ProblemNoDiskImage, Err: E.Cause(failure, "the Developer Disk Image is not mounted, and ", directory, " has no usable copy")}
	}
	return &Error{Problem: ProblemDiskImage, Err: failure}
}

func (s *Server) diskImageFiles(ctx context.Context, directory string, variant imagemounter.Variant) (string, error) {
	if imagemounter.Verify(ctx, directory, variant) == nil {
		return directory, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	own := filepath.Join(directory, variant.String())
	err := imagemounter.Verify(ctx, own, variant)
	if err == nil {
		return own, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", E.Cause(err, "verify ", variant)
}
