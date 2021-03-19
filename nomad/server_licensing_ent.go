// +build ent

package nomad

import (
	"errors"
	"fmt"
	"time"

	version "github.com/hashicorp/go-version"
	"github.com/hashicorp/nomad-licensing/license"
	nomadLicense "github.com/hashicorp/nomad-licensing/license"
	"github.com/hashicorp/nomad/nomad/structs"
)

func (es *EnterpriseState) FeatureCheck(feature license.Features, emitLog bool) error {
	if es.licenseWatcher == nil {
		return fmt.Errorf("license not initialized")
	}

	return es.licenseWatcher.FeatureCheck(feature, emitLog)
}

func (es *EnterpriseState) SetLicense(blob string, force bool) error {
	if es.licenseWatcher == nil {
		return fmt.Errorf("license watcher unable to set license")
	}

	return es.licenseWatcher.SetLicense(blob, force)
}

func (es *EnterpriseState) SetLicenseRequest(blob string, force bool) error {
	if es.licenseWatcher == nil {
		return fmt.Errorf("license watcher unable to set license")
	}

	return es.licenseWatcher.SetLicenseRequest(blob, force)
}

func (es *EnterpriseState) Features() uint64 {
	return uint64(es.licenseWatcher.Features())
}

// License returns the current license
func (es *EnterpriseState) License() *license.License {
	if es.licenseWatcher == nil {
		// everything is licensed while the watcher starts up
		return nil
	}
	return es.licenseWatcher.License()
}

// FileLicense returns the file license used to initialize the
// server. It may not be set or may not be the license currently in use.
func (es *EnterpriseState) FileLicenseOutdated() bool {
	if es.licenseWatcher == nil {
		// everything is licensed while the watcher starts up
		return false
	}

	fileLic := es.licenseWatcher.FileLicense()
	if fileLic == "" {
		return false
	}

	currentBlob := es.licenseWatcher.LicenseBlob()
	return fileLic != currentBlob
}

// ReloadLicense reloads the server's file license if the supplied license cfg
// is newer than the one currently in use
func (es *EnterpriseState) ReloadLicense(cfg *Config) error {
	if es.licenseWatcher == nil {
		return nil
	}
	licenseConfig := &LicenseConfig{
		LicenseEnvBytes: cfg.LicenseEnv,
		LicensePath:     cfg.LicensePath,
	}
	return es.licenseWatcher.Reload(licenseConfig)
}

// syncLeaderLicense propagates its in-memory license to raft if the proper
// conditions are met.
func (s *Server) syncLeaderLicense() {
	lic := s.EnterpriseState.License()
	if lic == nil || lic.Temporary {
		return
	}

	blob := s.EnterpriseState.licenseWatcher.LicenseBlob()

	stored, err := s.fsm.State().License(nil)
	if err != nil {
		s.logger.Error("received error retrieving raft license, syncing leader license")
	}

	// If raft already has the current license or one that was forcibly set
	// nothing to do
	if stored != nil && (stored.Signed == blob || stored.Force) {
		return
	}

	var raftLicense *nomadLicense.License
	if stored != nil {
		raftLicense, err = s.EnterpriseState.licenseWatcher.ValidateLicense(stored.Signed)
		if err != nil {
			s.logger.Error("received error validating current raft license, syncing leader license")
		}

		// The raft license was forcibly set, nothing to do
		if stored.Force {
			s.logger.Debug("current raft license forcibly set, not syncing leader license")
			return
		}
	}

	// If the raft license is newer than current license nothing to do
	if raftLicense != nil && raftLicense.IssueTime.After(lic.IssueTime) {
		s.logger.Debug("current raft license is newer than ")
		return
	}

	// Server license is newer than the one in raft, propagate.
	if err := s.propagateLicense(lic, blob, false); err != nil {
		s.logger.Error("unable to sync current leader license to raft", "err", err)
	}

}

func (s *Server) propagateLicense(lic *nomadLicense.License, signedBlob string, force bool) error {
	stored, err := s.fsm.State().License(nil)
	if err != nil {
		return err
	}

	if lic.Temporary {
		s.logger.Debug("license is temporary, not propagating to raft")
		return nil
	}

	if !s.IsLeader() {
		s.logger.Debug("server is not leader, not propagating to raft")
		return nil
	}

	if stored == nil || stored.Signed != signedBlob {
		newLicense := structs.StoredLicense{
			Signed: signedBlob,
			Force:  force,
		}
		req := structs.LicenseUpsertRequest{License: &newLicense}
		if _, _, err := s.raftApply(structs.LicenseUpsertRequestType, &req); err != nil {
			return err
		}
	}
	return nil
}

var minLicenseMetaVersion = version.Must(version.NewVersion("0.12.1"))

func (s *Server) initTmpLicenseBarrier() (int64, error) {
	if !ServersMeetMinimumVersion(s.Members(), minLicenseMetaVersion, false) {
		s.logger.Named("core").Debug("cannot initialize temporary license barrier until all servers are above minimum version", "min_version", minLicenseMetaVersion)
		return 0, fmt.Errorf("temporary license barrier cannot be created until all servers are above minimum version %s", minLicenseMetaVersion)
	}

	fsmState := s.fsm.State()
	existingMeta, err := fsmState.TmpLicenseBarrier(nil)
	if err != nil {
		s.logger.Named("core").Error("failed to get temporary license barrier", "error", err)
		return 0, err
	}

	// If tmp license barrier already exists nothing to do
	if existingMeta != nil {
		return existingMeta.CreateTime, nil
	}

	if !s.IsLeader() {
		return 0, errors.New("server is not current leader, cannot create temporary license barrier")
	}

	// Apply temporary license timestamp
	timestamp := time.Now().UnixNano()
	req := structs.TmpLicenseBarrier{CreateTime: timestamp}
	if _, _, err := s.raftApply(structs.TmpLicenseUpsertRequestType, req); err != nil {
		s.logger.Error("failed to initialize temporary license barrier", "error", err)
		return 0, err
	}
	return timestamp, nil
}
