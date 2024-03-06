// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package users

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-multierror"
)

type OsUser struct {
	User user.User
}

func (u *OsUser) UserId() string {
	return u.User.Uid
}

func (u *OsUser) GroupIds() ([]string, error) {
	return u.User.GroupIds()
}

var globalCache = newCache()

// Lookup returns the user.User entry associated with the given username.
//
// Values are cached up to 1 hour, or 1 minute for failure cases.
func Lookup(username string) (*user.User, error) {
	return globalCache.GetUser(username)
}

func LookupOsUser(username string) (*OsUser, error) {
	u, err := Lookup(username)
	if err != nil {
		return nil, err
	}

	// if err != nil {
	// 	return fmt.Errorf("failed to identify user %q: %v", username, err)
	// }
	return &OsUser{User: *u}, nil
}

// LookupUnix returns the UID, GID, and home directory for username or returns
// an error. ID values are int to work well with Go library functions.
//
// Will always fail on Windows and Plan 9.
func LookupUnix(username string) (int, int, string, error) {
	u, err := Lookup(username)
	if err != nil {
		return 0, 0, "", fmt.Errorf("error looking up user %q: %w", username, err)
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, "", fmt.Errorf("error parsing uid: %w", err)
	}

	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, "", fmt.Errorf("error parsing gid: %w", err)
	}

	return uid, gid, u.HomeDir, nil
}

// lock is used to serialize all user lookup at the process level, because
// some NSS implementations are not concurrency safe
var lock sync.Mutex

// internalLookupUser username while holding a global process lock.
func internalLookupUser(username string) (*user.User, error) {
	lock.Lock()
	defer lock.Unlock()
	return user.Lookup(username)
}

// Current returns the current user, acquired while holding a global process
// lock.
func Current() (*user.User, error) {
	lock.Lock()
	defer lock.Unlock()
	return user.Current()
}

// WriteFileFor is like os.WriteFile except if possible it chowns the file to
// the specified user (possibly from Task.User) and sets the permissions to
// 0o600.
//
// If chowning fails (either due to OS or Nomad being unprivileged), the file
// will be left world readable (0o666).
//
// On failure a multierror with both the original and fallback errors will be
// returned.
//
// See SocketFileFor if writing a unix socket file.
func WriteFileFor(path string, contents []byte, username string) error {
	// Don't even bother trying to chown to an empty username
	var origErr error
	if username != "" {
		origErr := writeFileFor(path, contents, username)
		if origErr == nil {
			// Success!
			return nil
		}
	}

	// Fallback to world readable
	if err := os.WriteFile(path, contents, 0o666); err != nil {
		if origErr != nil {
			// Return both errors
			return &multierror.Error{
				Errors: []error{origErr, err},
			}
		} else {
			return err
		}
	}

	return nil
}

func writeFileFor(path string, contents []byte, username string) error {
	uid, _, _, err := LookupUnix(username)
	if err != nil {
		return err
	}

	if err := os.WriteFile(path, contents, 0o600); err != nil {
		return err
	}

	if err := os.Chown(path, uid, -1); err != nil {
		// Delete the file so that the fallback method properly resets
		// permissions.
		_ = os.Remove(path)
		return err
	}

	return nil
}

// SocketFileFor creates a unix domain socket file on the specified path and,
// if possible, makes it usable by only the specified user. Failing that it
// will leave the socket open to all users. Non-fatal errors are logged.
//
// See WriteFileFor if writing a regular file.
func SocketFileFor(logger hclog.Logger, path, username string) (net.Listener, error) {
	if err := os.RemoveAll(path); err != nil {
		logger.Warn("error removing socket", "path", path, "error", err)
	}

	udsln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}

	if username != "" {
		// Try to set perms on socket file to least privileges.
		if err := setSocketOwner(path, username); err == nil {
			// Success! Exit early
			return udsln, nil
		}

		// This error is expected to always occur in some environments (Windows,
		// non-root agents), so don't log above Trace.
		logger.Trace("failed to set user on socket", "path", path, "user", username, "error", err)
	}

	// Opportunistic least privileges failed above, so make sure anyone can use
	// the socket.
	if err := os.Chmod(path, 0o666); err != nil {
		logger.Warn("error setting socket permissions", "path", path, "error", err)
	}

	return udsln, nil
}

func setSocketOwner(path, username string) error {
	uid, _, _, err := LookupUnix(username)
	if err != nil {
		return err
	}

	if err := os.Chown(path, uid, -1); err != nil {
		return err
	}

	if err := os.Chmod(path, 0o600); err != nil {
		// Awkward situation that is hopefully impossible to reach where we could
		// chown the socket but not change its mode.
		return err
	}

	return nil
}
