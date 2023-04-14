//go:build ent
// +build ent

package client

import (
	"testing"

	"github.com/hashicorp/nomad-licensing/license"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/shoenig/test/must"
)

func TestEnterpriseClient_InitialFeatures(t *testing.T) {
	ci.Parallel(t)
	log := testlog.HCLogger(t)
	c := newEnterpriseClient(log)

	must.NoError(t, c.FeatureCheck(license.FeatureAuditLogging, true))
	must.NoError(t, c.FeatureCheck(license.AllFeatures(), true))
}

func TestEnterpriseClient_FeatureCheck(t *testing.T) {
	ci.Parallel(t)
	log := testlog.HCLogger(t)
	c := newEnterpriseClient(log)

	// Empty out features
	c.SetFeatures(0)

	must.Error(t, c.FeatureCheck(license.FeatureAuditLogging, true))
	t1 := c.logTimes[license.FeatureAuditLogging]

	// Log again, ensure the time hasn't changed
	must.Error(t, c.FeatureCheck(license.FeatureAuditLogging, true))
	t2 := c.logTimes[license.FeatureAuditLogging]

	// Ensure the time hasn't changed
	must.Equal(t, t1, t2)

	// Set some features
	c.SetFeatures(uint64(license.FeatureAuditLogging | license.FeatureNamespaces))

	// Ensure no error
	must.NoError(t, c.FeatureCheck(license.FeatureAuditLogging, false))
}
