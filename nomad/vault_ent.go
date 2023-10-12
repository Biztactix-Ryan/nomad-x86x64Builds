//go:build ent
// +build ent

package nomad

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad-licensing/license"
	vapi "github.com/hashicorp/vault/api"
)

// vaultNamespaceHeaderName is the Vault header set to specify which namespace
// the request is indented for. This is copied from
// "github.com/hashicorp/vault/sdk/helper/consts" to avoid pulling in the
// entire SDK.
const vaultNamespaceHeaderName = "X-Vault-Namespace"

type VaultEntDelegate struct {
	l              hclog.Logger
	featureChecker license.FeatureChecker
}

func (e *VaultEntDelegate) clientForTask(v *vaultClient, namespace string) (*vapi.Client, error) {
	// If the requsted namespace equals the default namespace short-circuit
	currNs := v.client.Headers().Get(vaultNamespaceHeaderName)
	if currNs == namespace {
		return v.client, nil
	}

	// If multi-vault namespace is not licensed, return error
	if err := e.featureChecker.FeatureCheck(license.FeatureMultiVaultNamespaces, true); err != nil {
		return nil, err
	}

	taskClient, err := v.client.Clone()
	if err != nil {
		return nil, err
	}

	// Set namespace for token request
	taskClient.SetNamespace(namespace)
	taskClient.SetWrappingLookupFunc(v.getWrappingFn())
	taskClient.SetToken(v.token)

	return taskClient, nil
}
