// Package cmount implents a FUSE mounting system for rclone remotes.
//
// This uses the cgo based cgofuse library

// +build cmount
// +build cgo
// +build linux darwin freebsd windows

package cmount

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/billziss-gh/cgofuse/fuse"
	"github.com/ncw/rclone/cmd/mountlib"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/vfs"
	"github.com/ncw/rclone/vfs/vfsflags"
	"github.com/okzk/sdnotify"
	"github.com/pkg/errors"
)

func init() {
	name := "cmount"
	if runtime.GOOS == "windows" {
		name = "mount"
	}
	mountlib.NewMountCommand(name, Mount)
}

// mountOptions configures the options from the command line flags
func mountOptions(device string, mountpoint string) (options []string) {
	// Options
	options = []string{
		"-o", "fsname=" + device,
		"-o", "subtype=rclone",
		"-o", fmt.Sprintf("max_readahead=%d", mountlib.MaxReadAhead),
		"-o", fmt.Sprintf("attr_timeout=%g", mountlib.AttrTimeout.Seconds()),
		// This causes FUSE to supply O_TRUNC with the Open
		// call which is more efficient for cmount.  However
		// it does not work with cgofuse on Windows with
		// WinFSP so cmount must work with or without it.
		"-o", "atomic_o_trunc",
	}
	if mountlib.DebugFUSE {
		options = append(options, "-o", "debug")
	}

	// OSX options
	if runtime.GOOS == "darwin" {
		options = append(options, "-o", "volname="+mountlib.VolumeName)
		if mountlib.NoAppleDouble {
			options = append(options, "-o", "noappledouble")
		}
		if mountlib.NoAppleXattr {
			options = append(options, "-o", "noapplexattr")
		}
	}

	// Windows options
	if runtime.GOOS == "windows" {
		// These cause WinFsp to mean the current user
		options = append(options, "-o", "uid=-1")
		options = append(options, "-o", "gid=-1")
		options = append(options, "--FileSystemName=rclone")
	}

	if mountlib.AllowNonEmpty {
		options = append(options, "-o", "nonempty")
	}
	if mountlib.AllowOther {
		options = append(options, "-o", "allow_other")
	}
	if mountlib.AllowRoot {
		options = append(options, "-o", "allow_root")
	}
	if mountlib.DefaultPermissions {
		options = append(options, "-o", "default_permissions")
	}
	if vfsflags.Opt.ReadOnly {
		options = append(options, "-o", "ro")
	}
	if mountlib.WritebackCache {
		// FIXME? options = append(options, "-o", WritebackCache())
	}
	for _, option := range mountlib.ExtraOptions {
		options = append(options, "-o", option)
	}
	for _, option := range mountlib.ExtraFlags {
		options = append(options, option)
	}
	return options
}

// waitFor runs fn() until it returns true or the timeout expires
func waitFor(fn func() bool) (ok bool) {
	const totalWait = 10 * time.Second
	const individualWait = 10 * time.Millisecond
	for i := 0; i < int(totalWait/individualWait); i++ {
		ok = fn()
		if ok {
			return ok
		}
		time.Sleep(individualWait)
	}
	return false
}

// mount the file system
//
// The mount point will be ready when this returns.
//
// returns an error, and an error channel for the serve process to
// report an error when fusermount is called.
func mount(f fs.Fs, mountpoint string) (*vfs.VFS, <-chan error, func() error, error) {
	fs.Debugf(f, "Mounting on %q", mountpoint)

	// Check the mountpoint - in Windows the mountpoint musn't exist before the mount
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(mountpoint)
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "mountpoint")
		}
		if !fi.IsDir() {
			return nil, nil, nil, errors.New("mountpoint is not a directory")
		}
	}

	// Create underlying FS
	fsys := NewFS(f)
	host := fuse.NewFileSystemHost(fsys)

	// Create options
	options := mountOptions(f.Name()+":"+f.Root(), mountpoint)
	fs.Debugf(f, "Mounting with options: %q", options)

	// Serve the mount point in the background returning error to errChan
	errChan := make(chan error, 1)
	go func() {
		var err error
		ok := host.Mount(mountpoint, options)
		if !ok {
			err = errors.New("mount failed")
			fs.Errorf(f, "Mount failed")
		}
		errChan <- err
	}()

	// unmount
	unmount := func() error {
		// Shutdown the VFS
		fsys.VFS.Shutdown()
		fs.Debugf(nil, "Calling host.Unmount")
		if host.Unmount() {
			fs.Debugf(nil, "host.Unmount succeeded")
			if runtime.GOOS == "windows" {
				if !waitFor(func() bool {
					_, err := os.Stat(mountpoint)
					return err != nil
				}) {
					fs.Errorf(nil, "mountpoint %q didn't disappear after unmount - continuing anyway", mountpoint)
				}
			}
			return nil
		}
		fs.Debugf(nil, "host.Unmount failed")
		return errors.New("host unmount failed")
	}

	// Wait for the filesystem to become ready, checking the file
	// system didn't blow up before starting
	select {
	case err := <-errChan:
		err = errors.Wrap(err, "mount stopped before calling Init")
		return nil, nil, nil, err
	case <-fsys.ready:
	}

	// Wait for the mount point to be available on Windows
	// On Windows the Init signal comes slightly before the mount is ready
	if runtime.GOOS == "windows" {
		if !waitFor(func() bool {
			_, err := os.Stat(mountpoint)
			return err == nil
		}) {
			fs.Errorf(nil, "mountpoint %q didn't became available on mount - continuing anyway", mountpoint)
		}
	}

	return fsys.VFS, errChan, unmount, nil
}

// Mount mounts the remote at mountpoint.
//
// If noModTime is set then it
func Mount(f fs.Fs, mountpoint string) error {
	// Mount it
	FS, errChan, _, err := mount(f, mountpoint)
	if err != nil {
		return errors.Wrap(err, "failed to mount FUSE fs")
	}

	// Note cgofuse unmounts the fs on SIGINT etc

	sigHup := make(chan os.Signal, 1)
	signal.Notify(sigHup, syscall.SIGHUP)

	if err := sdnotify.SdNotifyReady(); err != nil && err != sdnotify.SdNotifyNoSocket {
		return errors.Wrap(err, "failed to notify systemd")
	}

waitloop:
	for {
		select {
		// umount triggered outside the app
		case err = <-errChan:
			break waitloop
		// user sent SIGHUP to clear the cache
		case <-sigHup:
			root, err := FS.Root()
			if err != nil {
				fs.Errorf(f, "Error reading root: %v", err)
			} else {
				root.ForgetAll()
			}
		}
	}

	_ = sdnotify.SdNotifyStopping()
	if err != nil {
		return errors.Wrap(err, "failed to umount FUSE fs")
	}

	return nil
}
