// +build ent

package nomad

import (
	"context"
	"fmt"
	"io/ioutil"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-licensing"
	nomadLicense "github.com/hashicorp/nomad-licensing/license"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"golang.org/x/time/rate"
)

var (
	ErrOlderLicense = fmt.Errorf("requested license is older than current one, use force to override")
)

const (
	// expiredTmpGrace is the grace period after a server is restarted with an expired
	// license to allow for a new valid license to be applied.
	// The value should be short enough to inconvenience unlicensed users,
	// but long enough to allow legitimate users to apply the new license
	// manually, e.g. upon upgrades from non-licensed versions.
	// This value gets evaluated twice to be extra cautious
	defaultExpiredTmpGrace = 1 * time.Minute

	// defaultMonitorLimit is the default rate limit for querying raft for a License
	defaultMonitorLimit = 1 * time.Second

	// defaultMonitorLimit is the default burst limit for querying raft for a License
	defaultMonitorBurst = 1

	// permanentLicenseID is the license ID used for permanent (s3) enterprise builds
	permanentLicenseID = "permanent"

	// Temporary Nomad-Enterprise licenses should operate for six hours
	temporaryLicenseTimeLimit = 6 * time.Hour

	licenseExpired = "license is no longer valid"
)

type stateFn func() *state.StateStore

// LicenseConfig allows for tunable licensing config
// primarily used for enterprise testing
type LicenseConfig struct {
	// LicenseEnvBytes is the license bytes to use for the server's license
	LicenseEnvBytes string

	// LicensePath is the path to use for the server's license
	LicensePath string

	// AdditionalPubKeys is a set of public keys to
	AdditionalPubKeys []string

	// PropagateFn is the function to be invoked when propagating a license to raft
	PropagateFn func(*nomadLicense.License, string, bool) (uint64, error)

	// ShutdownCallback is the function to be invoked when a temporary license
	// has expired and the server should be shutdown
	ShutdownCallback func() error

	// InitTmpLicenseBarrier establishes a barrier in raft for when a temporary
	// license first started. It is referenced to ensure that a server only
	// operates for the duration of a temporary license
	InitTmpLicenseBarrier func() (int64, error)

	StateStore stateFn

	Logger hclog.InterceptLogger

	// preventStart is used for testing to control when to start watcher
	preventStart bool
}

// ServerLicense contains an expanded license and its corresponding blob
type ServerLicense struct {
	license *nomadLicense.License
	blob    string
}

type LicenseWatcher struct {
	// license is the watchers atomically stored ServerLicense
	licenseInfo atomic.Value

	// fileLicense is the license loaded from the server's license path or env
	// it is set when the LicenseWatcher is initialized and when Reloaded.
	fileLicense string

	// shutdownFunc is a callback invoked when temporary license expires and server should shutdown
	shutdownCallback func() error

	// expiredTmpGrace is the duration to allow a valid license to be applied
	// when a server starts with a temporary license and a cluster age greater
	// than the temporaryLicenseTimeLimit
	expiredTmpGrace time.Duration

	// monitorTmpExpCtx is the context used to notify that the expired
	// temporary license monitor should stop
	monitorTmpExpCtx context.Context

	// monitorTmpExpCancel is used to stop the temporary license monitor from
	// shutting down a server. It is cancelled when a non temporary license is set
	monitorTmpExpCancel context.CancelFunc

	watcher *licensing.Watcher

	logMu  sync.Mutex
	logger hclog.Logger

	// logTimes tracks the last time a log message was sent for a feature
	logTimes map[nomadLicense.Features]time.Time

	// establishTmpLicenseBarrierFn is used to set a barrier in raft for when a
	// server first started using a temporary license
	establishTmpLicenseBarrierFn func() (int64, error)

	stateStoreFn stateFn

	preventStart bool

	propagateFn func(*nomadLicense.License, string, bool) (uint64, error)
}

func NewLicenseWatcher(cfg *LicenseConfig) (*LicenseWatcher, error) {
	// Check for file license
	fileLicense, err := licenseFromLicenseConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to read license from config: %w", err)
	}

	// Initialize temporary license
	_, tmpSigned, tmpPubKey, err := temporaryLicenseInfo()
	if err != nil {
		return nil, fmt.Errorf("failed creating temporary license: %w", err)
	}
	cfg.AdditionalPubKeys = append(cfg.AdditionalPubKeys, tmpPubKey)

	ctx, cancel := context.WithCancel(context.Background())

	lw := &LicenseWatcher{
		fileLicense:                  fileLicense,
		shutdownCallback:             cfg.ShutdownCallback,
		expiredTmpGrace:              defaultExpiredTmpGrace,
		monitorTmpExpCtx:             ctx,
		monitorTmpExpCancel:          cancel,
		logger:                       cfg.Logger.Named("licensing"),
		logTimes:                     make(map[nomadLicense.Features]time.Time),
		establishTmpLicenseBarrierFn: cfg.InitTmpLicenseBarrier,
		propagateFn:                  cfg.PropagateFn,
		stateStoreFn:                 cfg.StateStore,
		preventStart:                 cfg.preventStart,
	}

	opts := &licensing.WatcherOptions{
		ProductName:          nomadLicense.ProductName,
		InitLicense:          tmpSigned,
		AdditionalPublicKeys: cfg.AdditionalPubKeys,
	}

	// Create the new watcher with options
	watcher, _, err := licensing.NewWatcher(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize nomad license: %w", err)
	}
	lw.watcher = watcher

	// Try to set the file license
	initLicense := tmpSigned
	if fileLicense != "" {
		if _, err := lw.watcher.SetLicense(fileLicense); err != nil {
			if !strings.Contains(err.Error(), licenseExpired) {
				// If the error is an expired license, continue on in case a valid
				// one is in raft. If it's invalid, error out
				return nil, fmt.Errorf("a file license was configured but the license is invalid: %w", err)
			}
			lw.logger.Warn("Configured enterprise license file is expired! Falling back to temporary license. Please update, or remove license configuration if setting the license via CLI/API")
		} else {
			// update the initLicense if license was set
			initLicense = fileLicense
		}
	}

	startUpLicense, err := lw.watcher.License()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve startup license: %w", err)
	}

	license, err := nomadLicense.NewLicense(startUpLicense)
	if err != nil {
		return nil, fmt.Errorf("failed to convert startup license: %w", err)
	}

	// Store the expanded license and the corresponding blob
	lw.licenseInfo.Store(&ServerLicense{
		license: license,
		blob:    initLicense,
	})

	return lw, nil
}

// Reload updates the license from the config
func (w *LicenseWatcher) Reload(cfg *LicenseConfig) error {
	blob, err := licenseFromLicenseConfig(cfg)
	if err != nil {
		return err
	}

	if blob == "" {
		return nil
	}

	// Update the watchers fileLicense reference
	w.fileLicense = blob

	return w.SetLicense(blob, false)
}

// License atomically returns the license watchers stored license
func (w *LicenseWatcher) License() *nomadLicense.License {
	return w.licenseInfo.Load().(*ServerLicense).license
}

func (w *LicenseWatcher) LicenseBlob() string {
	return w.licenseInfo.Load().(*ServerLicense).blob
}

// FileLicense returns the watchers file license that was used to initialize
// the server. It is not necessarily the license that the server is currently using
// if a newer license was added via raft or manual API invocation
func (w *LicenseWatcher) FileLicense() string {
	return w.fileLicense
}

// ValidateLicense validates that the given blob is a valid go-licensing
// license as well as a valid nomad license
func (w *LicenseWatcher) ValidateLicense(blob string) (*nomadLicense.License, error) {
	lic, err := w.watcher.ValidateLicense(blob)
	if err != nil {
		return nil, err
	}
	nLic, err := nomadLicense.NewLicense(lic)
	if err != nil {
		return nil, err
	}
	return nLic, nil
}

// SetLicense sets the server's license and propagates the license via the
// watcher's propagateFn if set.
// The license will only be set if the passed license issue date is newer than
// the current license, or if the force flag is set.
func (w *LicenseWatcher) SetLicense(blob string, force bool) error {
	_, err := w.setLicense(blob, force, false)
	return err
}

// SetLicenseRequest is used to set the server's license when a operator has
// requested to do so. SetLicenseRequest differs from SetLicense in that it
// does not check if the current license in raft was forcibly set. Allowing the
// Operator to set a newer license ontop of a forcibly set license without
// using the force flag
func (w *LicenseWatcher) SetLicenseRequest(blob string, force bool) (uint64, error) {
	return w.setLicense(blob, force, true)
}

func (w *LicenseWatcher) setLicense(blob string, force, userReq bool) (uint64, error) {
	// Remove any newlines from the license
	blob = strings.TrimRight(blob, "\r\n")

	newLicense, err := w.watcher.ValidateLicense(blob)
	if err != nil {
		return 0, fmt.Errorf("error validating license: %w", err)
	}

	newNomadLic, err := nomadLicense.NewLicense(newLicense)
	if err != nil {
		return 0, fmt.Errorf("unable to create nomad specific license: %w", err)
	}

	// If we are forcibly setting, the current license is expired or nil just
	// set the license
	current := w.License()
	if current == nil || force || time.Now().After(current.TerminationTime) {
		goto SET_LICENSE
	}

	if current.Equal(newNomadLic.License) {
		return 0, nil
	}

	if !current.Temporary && !newNomadLic.IssueTime.After(current.IssueTime) {
		return 0, ErrOlderLicense
	}

	// If the current raft license was forcibly set and force was not set for
	// this request, do not set
	if !userReq {
		stored, err := w.stateStoreFn().License(nil)
		if err != nil {
			w.logger.Error("error getting license from raft %w", err)
		}

		if stored != nil && stored.Force && !force {
			w.logger.Debug("stored license forcibly set, ignoring non-force set request")
			return 0, fmt.Errorf("stored license forcibly set, must use force to override again")
		}
	}

SET_LICENSE:
	_, err = w.watcher.SetLicense(blob)
	if err != nil {
		return 0, err
	}

	var index uint64
	if !newNomadLic.Temporary && w.propagateFn != nil {
		modifyIdx, err := w.propagateFn(newNomadLic, blob, force)
		if err != nil {
			return 0, err
		}
		index = modifyIdx
	}

	w.licenseInfo.Store(&ServerLicense{
		license: newNomadLic,
		blob:    blob,
	})

	return index, nil
}

func (w *LicenseWatcher) Features() nomadLicense.Features {
	lic := w.License()
	if lic == nil {
		return nomadLicense.FeatureNone
	}

	// check if our local license has expired
	if time.Now().After(lic.TerminationTime) {
		return nomadLicense.FeatureNone
	}

	return lic.Features
}

// FeatureCheck determines if the given feature is included in License
// if emitLog is true, a log will only be sent once ever 5 minutes per feature
func (w *LicenseWatcher) FeatureCheck(feature nomadLicense.Features, emitLog bool) error {
	if w.hasFeature(feature) {
		return nil
	}

	err := fmt.Errorf("Feature %q is unlicensed", feature.String())

	if emitLog {
		// Only send log messages for a missing feature every 5 minutes
		w.logMu.Lock()
		defer w.logMu.Unlock()
		lastTime := w.logTimes[feature]
		now := time.Now()
		if now.Sub(lastTime) > 5*time.Minute {
			w.logger.Warn(err.Error())
			w.logTimes[feature] = now
		}
	}

	return err
}

func (w *LicenseWatcher) hasFeature(feature nomadLicense.Features) bool {
	return w.Features().HasFeature(feature)
}

// start the license watching process in a goroutine. Callers are responsible
// for ensuring it is shut down properly
func (w *LicenseWatcher) start(ctx context.Context) {
	if w.preventStart {
		return
	}

	go w.monitorWatcher(ctx)
	go w.temporaryLicenseMonitor(ctx)
	go w.monitorRaft(ctx)
}

// monitorWatcher monitors the LicenseWatchers go-licensing watcher
//
// Nomad uses the go licensing watcher channels mostly to log, and to stop the
// temporaryLicenseMonitor when a valid license has been applied.  Since Nomad
// does not shut down when a license has expired the ErrorCh simply logs.
func (w *LicenseWatcher) monitorWatcher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			w.watcher.Stop()
			return
		// Handle updated license from the watcher
		case <-w.watcher.UpdateCh():
			// Check if server is shutting down
			select {
			case <-ctx.Done():
				return
			default:
			}
			w.logger.Debug("received update from license manager")

			// Stop the temporaryLicenseMonitor now that we have a valid license
			w.monitorTmpExpCancel()
		// Handle licensing watcher errors, primarily expirations.
		case err := <-w.watcher.ErrorCh():
			w.logger.Error("license expired, please update license", "error", err) //TODO: more info

		case warnLicense := <-w.watcher.WarningCh():
			w.logger.Warn("license expiring", "time_left", time.Until(warnLicense.ExpirationTime).Truncate(time.Second))
		}
	}
}

func (w *LicenseWatcher) monitorRaft(ctx context.Context) {
	limiter := rate.NewLimiter(rate.Limit(defaultMonitorLimit), defaultMonitorBurst)

	var lastSigned string
	for {
		if err := limiter.Wait(ctx); err != nil {
			return
		}

		update, lic, err := w.waitForLicenseUpdate(ctx, lastSigned)
		if err != nil {
			w.logger.Warn("License sync error (will retry)", "error", err)
			select {
			case <-ctx.Done():
				return
			default:
			}
		}

		if update {
			err = w.SetLicense(lic.Signed, lic.Force)
			if err != nil && err != ErrOlderLicense {
				w.logger.Error("failed to set license from update", "error", err)
				// Only retry if license in raft is still potentially valid
				if !strings.Contains(err.Error(), "license is no longer valid") {
					continue
				}
			}
			lastSigned = lic.Signed
		}
	}
}

func (w *LicenseWatcher) waitForLicenseUpdate(ctx context.Context, lastSigned string) (bool, *structs.StoredLicense, error) {
	state := w.stateStoreFn()
	ws := state.NewWatchSet()
	ws.Add(ctx.Done())

	// Perform initial query, attaching watchset
	lic, err := state.License(ws)
	if err != nil {
		return false, nil, err
	}

	if lic != nil && lic.Signed != lastSigned {
		return true, lic, nil
	}

	// Wait for trigger
	ws.Watch(nil)

	updateLic, err := w.stateStoreFn().License(ws)
	if updateLic != nil && updateLic.Signed != lastSigned {
		return true, updateLic, err
	} else if updateLic == nil && lic != nil {
		return true, nil, err
	} else {
		return false, nil, err
	}
}

func (w *LicenseWatcher) temporaryLicenseMonitor(ctx context.Context) {
	state := w.stateStoreFn()
	// Block for the temporary license barrier to be populated to raft
	// This value is used to check if a temporary license is within
	// the initial 6 hour evaluation period
	tmpLicenseBarrier := w.getOrSetTmpLicenseBarrier(ctx, state)
	w.logger.Info("received temporary license barrier", "init time", tmpLicenseBarrier)

	for {
		tmpLicenseTooOld := time.Now().After(tmpLicenseBarrier.Add(temporaryLicenseTimeLimit))
		license := w.License()

		// Paid license, stop temporary license monitor
		if license != nil && !license.Temporary {
			w.logger.Debug("license is not temporary, temporary license monitor exiting")
			w.monitorTmpExpCancel()
			return
		}

		// Temporary license is too old, start monitoring expired temp license
		if license != nil && license.Temporary && tmpLicenseTooOld {
			go w.monitorExpiredTmpLicense(ctx)
			return
		}

		// Wait until the temporary license expiration
		exp := tmpLicenseBarrier.Add(temporaryLicenseTimeLimit)
		select {
		case <-time.After(exp.Sub(time.Now())):
			continue
		case <-ctx.Done():
			return
		}
	}

}

func (w *LicenseWatcher) monitorExpiredTmpLicense(ctx context.Context) {
	w.logger.Warn(
		`temporary license too old for evaluation period. Nomad will
	wait an additional grace period for valid Enterprise license to be applied
	before shutting down`, "grace period", w.expiredTmpGrace,
	)
	defer w.monitorTmpExpCancel()
	// Grace period for server and raft to initialize
	select {
	case <-ctx.Done():
		return
	case <-w.monitorTmpExpCtx.Done():
		w.logger.Info("received license update, stopping temporary license monitor")
		return
	case <-time.After(w.expiredTmpGrace):
		// A license was never applied
		if w.License().Temporary {
			w.logger.Error("cluster age is greater than temporary license lifespan. Please apply a valid license")
			w.logger.Error("cluster will shutdown soon. Please apply a valid license")
		} else {
			// This shouldn't happen but being careful to prevent bad shutdown
			w.logger.Error("never received monitor ctx cancellation update but license is no longer temporary")
			return
		}
	}

	// Wait once more for valid license to be applied before shutting down
	select {
	case <-ctx.Done():
		return
	case <-w.monitorTmpExpCtx.Done():
		w.logger.Info("license applied, cancelling expired temporary license shutdown")
	case <-time.After(w.expiredTmpGrace):
		w.logger.Error("temporary license grace period expired. shutting down")
		go w.shutdownCallback()
	}
}

func (w *LicenseWatcher) getOrSetTmpLicenseBarrier(ctx context.Context, state *state.StateStore) time.Time {
	interval := time.After(0 * time.Second)
	for {
		select {
		case <-ctx.Done():
			w.logger.Warn("context cancelled while waiting for temporary license barrier")
			return time.Now()
		case err := <-w.watcher.ErrorCh():
			w.logger.Warn("received error from license watcher", "err", err)
			return time.Now()
		case <-interval:
			tmpCreateTime, err := w.establishTmpLicenseBarrierFn()
			if err != nil {
				w.logger.Info("failed to get or set temporary license barrier, retrying...", "error", err)
				interval = time.After(2 * time.Second)
				continue
			}
			return time.Unix(0, tmpCreateTime)
		}
	}
}

func licenseFromLicenseConfig(cfg *LicenseConfig) (string, error) {
	if cfg.LicenseEnvBytes != "" {
		return cfg.LicenseEnvBytes, nil
	}

	if cfg.LicensePath != "" {
		licRaw, err := ioutil.ReadFile(cfg.LicensePath)
		if err != nil {
			return "", fmt.Errorf("failed to read license file %w", err)
		}
		return strings.TrimRight(string(licRaw), "\r\n"), nil
	}

	return "", nil
}

// tempLicenseTooOld checks if the cluster age is older than the temporary
// license time limit
func tmpLicenseTooOld(originalTmpCreate time.Time) bool {
	return time.Now().After(originalTmpCreate.Add(temporaryLicenseTimeLimit))
}
