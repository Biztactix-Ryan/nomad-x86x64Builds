// +build ent

package agent

import (
	"fmt"
	"path/filepath"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/audit"
	"github.com/hashicorp/nomad/command/agent/event"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/pkg/errors"
)

// Ensure audit.Auditor is an Eventer
var _ event.Eventer = &audit.Auditor{}

func (a *Agent) setupEnterpriseAgent(logger hclog.InterceptLogger) error {
	// Ensure we have at least empty Auditor config
	if a.config.Audit == nil {
		a.config.Audit = DefaultConfig().Audit
	}

	// Setup auditor
	auditor, err := a.setupAuditor(a.config.Audit, logger)
	if err != nil {
		return errors.Wrap(err, "error configuring auditor")
	}

	// set eventer
	a.eventer = auditor

	return nil
}

func (a *Agent) setupAuditor(cfg *config.AuditConfig, logger hclog.InterceptLogger) (*audit.Auditor, error) {
	var enabled bool
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}

	var filters []audit.FilterConfig
	for _, f := range cfg.Filters {
		filterType := audit.FilterType(f.Type)
		if !filterType.Valid() {
			return nil, fmt.Errorf("Invalid filter type %s", f.Type)
		}

		filter := audit.FilterConfig{
			Type:       filterType,
			Endpoints:  f.Endpoints,
			Stages:     f.Stages,
			Operations: f.Operations,
		}
		filters = append(filters, filter)
	}

	var sinks []audit.SinkConfig
	for _, s := range cfg.Sinks {
		// Split file name from path
		dir, file := filepath.Split(s.Path)

		// If path is provided, but no filename, then default is used.
		if file == "" {
			file = "audit.log"
		}

		// Check that the sink type is valid
		sinkType := audit.SinkType(s.Type)
		if !sinkType.Valid() {
			return nil, fmt.Errorf("Invalid sink type %s", s.Type)
		}

		// Check that the sink format is valid
		sinkFmt := audit.SinkFormat(s.Format)
		if !sinkFmt.Valid() {
			return nil, fmt.Errorf("Invalid sink format %s", s.Format)
		}

		// Set default delivery guarantee to enforced
		if s.DeliveryGuarantee == "" {
			s.DeliveryGuarantee = "enforced"
		}

		delivery := audit.RunMode(s.DeliveryGuarantee)
		if !delivery.Valid() {
			return nil, fmt.Errorf("Invalid delivery guarantee %s", s.DeliveryGuarantee)
		}

		sink := audit.SinkConfig{
			Name:              s.Name,
			Type:              sinkType,
			DeliveryGuarantee: delivery,
			Format:            sinkFmt,
			FileName:          file,
			Path:              dir,
			RotateDuration:    s.RotateDuration,
			RotateBytes:       s.RotateBytes,
			RotateMaxFiles:    s.RotateMaxFiles,
		}
		sinks = append(sinks, sink)
	}

	// Configure default sink if none are configured
	if len(sinks) == 0 {
		defaultSink := audit.SinkConfig{
			Name:     "default-sink",
			Type:     audit.FileSink,
			Format:   audit.JSONFmt,
			FileName: "audit.log",
			Path:     filepath.Join(a.config.DataDir, "audit"),
		}
		sinks = append(sinks, defaultSink)
	}

	auditCfg := &audit.Config{
		Enabled: enabled,
		Filters: filters,
		Sinks:   sinks,
		Logger:  logger.ResetNamedIntercept("audit"),
	}

	auditor, err := audit.NewAuditor(auditCfg)
	if err != nil {
		return nil, err
	}

	return auditor, nil
}

// entReloadEventer enables or disables the eventer and calls reopen.
// Assumes caller has nil checked cfg
func (a *Agent) entReloadEventer(cfg *config.AuditConfig) error {
	enabled := cfg.Enabled != nil && *cfg.Enabled
	a.eventer.SetEnabled(enabled)

	if err := a.eventer.Reopen(); err != nil {
		return err
	}

	return nil
}
