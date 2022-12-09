package config

import (
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/hashicorp/nomad/nomad/structs/config"
)

// ArtifactConfig is the internal readonly copy of the client agent's
// ArtifactConfig.
type ArtifactConfig struct {
	HTTPReadTimeout time.Duration
	HTTPMaxBytes    int64

	GCSTimeout time.Duration
	GitTimeout time.Duration
	HgTimeout  time.Duration
	S3Timeout  time.Duration

	DisableFilesystemIsolation bool
}

// ArtifactConfigFromAgent creates a new internal readonly copy of the client
// agent's ArtifactConfig. The config should have already been validated.
func ArtifactConfigFromAgent(c *config.ArtifactConfig) (*ArtifactConfig, error) {
	httpReadTimeout, err := time.ParseDuration(*c.HTTPReadTimeout)
	if err != nil {
		return nil, fmt.Errorf("error parsing HTTPReadTimeout: %w", err)
	}

	httpMaxSize, err := humanize.ParseBytes(*c.HTTPMaxSize)
	if err != nil {
		return nil, fmt.Errorf("error parsing HTTPMaxSize: %w", err)
	}

	gcsTimeout, err := time.ParseDuration(*c.GCSTimeout)
	if err != nil {
		return nil, fmt.Errorf("error parsing GCSTimeout: %w", err)
	}

	gitTimeout, err := time.ParseDuration(*c.GitTimeout)
	if err != nil {
		return nil, fmt.Errorf("error parsing GitTimeout: %w", err)
	}

	hgTimeout, err := time.ParseDuration(*c.HgTimeout)
	if err != nil {
		return nil, fmt.Errorf("error parsing HgTimeout: %w", err)
	}

	s3Timeout, err := time.ParseDuration(*c.S3Timeout)
	if err != nil {
		return nil, fmt.Errorf("error parsing S3Timeout: %w", err)
	}

	return &ArtifactConfig{
		HTTPReadTimeout:            httpReadTimeout,
		HTTPMaxBytes:               int64(httpMaxSize),
		GCSTimeout:                 gcsTimeout,
		GitTimeout:                 gitTimeout,
		HgTimeout:                  hgTimeout,
		S3Timeout:                  s3Timeout,
		DisableFilesystemIsolation: *c.DisableFilesystemIsolation,
	}, nil
}

func (a *ArtifactConfig) Copy() *ArtifactConfig {
	if a == nil {
		return nil
	}

	newCopy := *a
	return &newCopy
}
