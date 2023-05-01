//go:build ent
// +build ent

package nomad

import (
	"strings"
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearch_PrefixSearch_Quota(t *testing.T) {
	ci.Parallel(t)
	assert := assert.New(t)
	s, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanup()

	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	qs := mock.QuotaSpec()
	assert.Nil(s.fsm.State().UpsertQuotaSpecs(2000, []*structs.QuotaSpec{qs}))

	prefix := qs.Name[:len(qs.Name)-2]

	req := &structs.SearchRequest{
		Prefix:  prefix,
		Context: structs.Quotas,
		QueryOptions: structs.QueryOptions{
			Region: "global",
		},
	}

	var resp structs.SearchResponse
	if err := msgpackrpc.CallWithCodec(codec, "Search.PrefixSearch", req, &resp); err != nil {
		t.Fatalf("err: %v", err)
	}

	assert.Equal(1, len(resp.Matches[structs.Quotas]))
	assert.Equal(qs.Name, resp.Matches[structs.Quotas][0])
	assert.Equal(resp.Truncations[structs.Quotas], false)

	assert.Equal(uint64(2000), resp.Index)
}

func TestSearch_PrefixSearch_Quota_ACL(t *testing.T) {
	ci.Parallel(t)
	assert := assert.New(t)
	s, root, cleanupS1 := TestACLServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanupS1()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)
	state := s.fsm.State()

	quota := mock.QuotaSpec()
	assert.Nil(state.UpsertQuotaSpecs(490, []*structs.QuotaSpec{quota}))

	ns := mock.Namespace()
	ns.Quota = quota.Name
	assert.Nil(state.UpsertNamespaces(500, []*structs.Namespace{ns}))

	job1 := mock.Job()
	assert.Nil(state.UpsertJob(structs.MsgTypeTestSetup, 502, job1))

	job2 := mock.Job()
	job2.Namespace = ns.Name
	assert.Nil(state.UpsertJob(structs.MsgTypeTestSetup, 504, job2))

	assert.Nil(state.UpsertNode(structs.MsgTypeTestSetup, 1001, mock.Node()))

	req := &structs.SearchRequest{
		Prefix:  "",
		Context: structs.Jobs,
		QueryOptions: structs.QueryOptions{
			Region:    "global",
			Namespace: job1.Namespace,
		},
	}

	// Try without a token and expect failure
	{
		var resp structs.SearchResponse
		err := msgpackrpc.CallWithCodec(codec, "Search.PrefixSearch", req, &resp)
		assert.NotNil(err)
		assert.Equal(err.Error(), structs.ErrPermissionDenied.Error())
	}

	// Try with a quota:read token and expect success due to All context
	{
		validToken := mock.CreatePolicyAndToken(t, state, 1007, "test-valid", mock.QuotaPolicy(acl.PolicyRead))
		req.Context = structs.All
		req.AuthToken = validToken.SecretID
		var resp structs.SearchResponse
		assert.Nil(msgpackrpc.CallWithCodec(codec, "Search.PrefixSearch", req, &resp))
		assert.Equal(uint64(490), resp.Index)
		assert.Len(resp.Matches[structs.Quotas], 1)
		assert.Equal(quota.Name, resp.Matches[structs.Quotas][0])

		// Others filtered out since token only has access to quota:read
		assert.Len(resp.Matches[structs.Jobs], 0)
		assert.Len(resp.Matches[structs.Namespaces], 0)
		assert.Len(resp.Matches[structs.Nodes], 0)
	}

	// Try with a valid token for non-default namespace:read-job
	{
		validToken := mock.CreatePolicyAndToken(t, state, 1009, "test-valid2",
			mock.NamespacePolicy(job2.Namespace, "", []string{acl.NamespaceCapabilityReadJob}))
		req.AuthToken = validToken.SecretID
		req.Namespace = job2.Namespace
		var resp structs.SearchResponse
		assert.Nil(msgpackrpc.CallWithCodec(codec, "Search.PrefixSearch", req, &resp))
		assert.Len(resp.Matches[structs.Jobs], 1)
		assert.Equal(job2.ID, resp.Matches[structs.Jobs][0])

		// Index of job - not node - because node context is filtered out
		assert.Equal(uint64(504), resp.Index)

		// Others filtered out since token only has access to namespace:read-job
		assert.Len(resp.Matches[structs.Nodes], 0)
		assert.Len(resp.Matches[structs.Quotas], 0)
	}

	// Try with a valid token for quota:read, node:read, and default
	// namespace:read-job
	{
		validToken := mock.CreatePolicyAndToken(t, state, 1011, "test-valid3", strings.Join([]string{
			mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilityReadJob}),
			mock.NodePolicy(acl.PolicyRead),
			mock.QuotaPolicy(acl.PolicyRead),
		}, "\n"))
		req.AuthToken = validToken.SecretID
		req.Namespace = structs.DefaultNamespace
		var resp structs.SearchResponse
		assert.Nil(msgpackrpc.CallWithCodec(codec, "Search.PrefixSearch", req, &resp))
		assert.Len(resp.Matches[structs.Jobs], 1)
		assert.Equal(job1.ID, resp.Matches[structs.Jobs][0])
		assert.Len(resp.Matches[structs.Nodes], 1)
		assert.Equal(uint64(1001), resp.Index)
		assert.Len(resp.Matches[structs.Namespaces], 1)
		assert.Len(resp.Matches[structs.Quotas], 1)
	}

	// Try with a management token
	{
		req.AuthToken = root.SecretID
		var resp structs.SearchResponse
		assert.Nil(msgpackrpc.CallWithCodec(codec, "Search.PrefixSearch", req, &resp))
		assert.Equal(uint64(1001), resp.Index)
		assert.Len(resp.Matches[structs.Jobs], 1)
		assert.Equal(job1.ID, resp.Matches[structs.Jobs][0])
		assert.Len(resp.Matches[structs.Nodes], 1)
		assert.Len(resp.Matches[structs.Namespaces], 2)
		assert.Len(resp.Matches[structs.Quotas], 1)
	}
}

func TestSearch_PrefixSearch_Recommendation(t *testing.T) {
	ci.Parallel(t)
	assert := assert.New(t)
	s, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanup()

	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)
	state := s.fsm.State()

	job := mock.Job()
	rec := mock.Recommendation(job)
	require.NoError(t, state.UpsertJob(structs.MsgTypeTestSetup, 1000, job))
	require.NoError(t, state.UpsertRecommendation(1000, rec))

	prefix := rec.ID[:len(rec.ID)-2]

	req := &structs.SearchRequest{
		Prefix:  prefix,
		Context: structs.Recommendations,
		QueryOptions: structs.QueryOptions{
			Namespace: structs.DefaultNamespace,
			Region:    "global",
		},
	}

	var resp structs.SearchResponse
	require.NoError(t, msgpackrpc.CallWithCodec(codec, "Search.PrefixSearch", req, &resp))

	assert.Equal(1, len(resp.Matches[structs.Recommendations]))
	assert.Equal(rec.ID, resp.Matches[structs.Recommendations][0])
	assert.Equal(resp.Truncations[structs.Recommendations], false)

	assert.EqualValues(uint64(1000), resp.Index)
}
