package structs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/go-set"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"golang.org/x/crypto/blake2b"
	"golang.org/x/exp/slices"
)

const (
	// ACLUpsertPoliciesRPCMethod is the RPC method for batch creating or
	// modifying ACL policies.
	//
	// Args: ACLPolicyUpsertRequest
	// Reply: GenericResponse
	ACLUpsertPoliciesRPCMethod = "ACL.UpsertPolicies"

	// ACLUpsertTokensRPCMethod is the RPC method for batch creating or
	// modifying ACL tokens.
	//
	// Args: ACLTokenUpsertRequest
	// Reply: ACLTokenUpsertResponse
	ACLUpsertTokensRPCMethod = "ACL.UpsertTokens"

	// ACLDeleteTokensRPCMethod is the RPC method for batch deleting ACL
	// tokens.
	//
	// Args: ACLTokenDeleteRequest
	// Reply: GenericResponse
	ACLDeleteTokensRPCMethod = "ACL.DeleteTokens"

	// ACLUpsertRolesRPCMethod is the RPC method for batch creating or
	// modifying ACL roles.
	//
	// Args: ACLRolesUpsertRequest
	// Reply: ACLRolesUpsertResponse
	ACLUpsertRolesRPCMethod = "ACL.UpsertRoles"

	// ACLDeleteRolesByIDRPCMethod the RPC method for batch deleting ACL
	// roles by their ID.
	//
	// Args: ACLRolesDeleteByIDRequest
	// Reply: ACLRolesDeleteByIDResponse
	ACLDeleteRolesByIDRPCMethod = "ACL.DeleteRolesByID"

	// ACLListRolesRPCMethod is the RPC method for listing ACL roles.
	//
	// Args: ACLRolesListRequest
	// Reply: ACLRolesListResponse
	ACLListRolesRPCMethod = "ACL.ListRoles"

	// ACLGetRolesByIDRPCMethod is the RPC method for detailing a number of ACL
	// roles using their ID. This is an internal only RPC endpoint and used by
	// the ACL Role replication process.
	//
	// Args: ACLRolesByIDRequest
	// Reply: ACLRolesByIDResponse
	ACLGetRolesByIDRPCMethod = "ACL.GetRolesByID"

	// ACLGetRoleByIDRPCMethod is the RPC method for detailing an individual
	// ACL role using its ID.
	//
	// Args: ACLRoleByIDRequest
	// Reply: ACLRoleByIDResponse
	ACLGetRoleByIDRPCMethod = "ACL.GetRoleByID"

	// ACLGetRoleByNameRPCMethod is the RPC method for detailing an individual
	// ACL role using its name.
	//
	// Args: ACLRoleByNameRequest
	// Reply: ACLRoleByNameResponse
	ACLGetRoleByNameRPCMethod = "ACL.GetRoleByName"

	// ACLUpsertAuthMethodsRPCMethod is the RPC method for batch creating or
	// modifying auth methods.
	//
	// Args: ACLAuthMethodsUpsertRequest
	// Reply: ACLAuthMethodUpsertResponse
	ACLUpsertAuthMethodsRPCMethod = "ACL.UpsertAuthMethods"

	// ACLDeleteAuthMethodsRPCMethod is the RPC method for batch deleting auth
	// methods.
	//
	// Args: ACLAuthMethodDeleteRequest
	// Reply: ACLAuthMethodDeleteResponse
	ACLDeleteAuthMethodsRPCMethod = "ACL.DeleteAuthMethods"

	// ACLListAuthMethodsRPCMethod is the RPC method for listing auth methods.
	//
	// Args: ACLAuthMethodListRequest
	// Reply: ACLAuthMethodListResponse
	ACLListAuthMethodsRPCMethod = "ACL.ListAuthMethods"

	// ACLGetAuthMethodRPCMethod is the RPC method for detailing an individual
	// auth method using its name.
	//
	// Args: ACLAuthMethodGetRequest
	// Reply: ACLAuthMethodGetResponse
	ACLGetAuthMethodRPCMethod = "ACL.GetAuthMethod"

	// ACLGetAuthMethodsRPCMethod is the RPC method for getting multiple auth
	// methods using their names.
	//
	// Args: ACLAuthMethodsGetRequest
	// Reply: ACLAuthMethodsGetResponse
	ACLGetAuthMethodsRPCMethod = "ACL.GetAuthMethods"
)

const (
	// ACLMaxExpiredBatchSize is the maximum number of expired ACL tokens that
	// will be garbage collected in a single trigger. This number helps limit
	// the replication pressure due to expired token deletion. If there are a
	// large number of expired tokens pending garbage collection, this value is
	// a potential limiting factor.
	ACLMaxExpiredBatchSize = 4096

	// maxACLRoleDescriptionLength limits an ACL roles description length.
	maxACLRoleDescriptionLength = 256
)

var (
	// validACLRoleName is used to validate an ACL role name.
	validACLRoleName = regexp.MustCompile("^[a-zA-Z0-9-]{1,128}$")

	// validACLAuthMethodName is used to validate an ACL auth method name.
	validACLAuthMethod = regexp.MustCompile("^[a-zA-Z0-9-]{1,128}$")
)

// ACLTokenRoleLink is used to link an ACL token to an ACL role. The ACL token
// can therefore inherit all the ACL policy permissions that the ACL role
// contains.
type ACLTokenRoleLink struct {

	// ID is the ACLRole.ID UUID. This field is immutable and represents the
	// absolute truth for the link.
	ID string

	// Name is the human friendly identifier for the ACL role and is a
	// convenience field for operators. This field is always resolved to the
	// ID and discarded before the token is stored in state. This is because
	// operators can change the name of an ACL role.
	Name string
}

// Canonicalize performs basic canonicalization on the ACL token object. It is
// important for callers to understand certain fields such as AccessorID are
// set if it is empty, so copies should be taken if needed before calling this
// function.
func (a *ACLToken) Canonicalize() {

	// If the accessor ID is empty, it means this is creation of a new token,
	// therefore we need to generate base information.
	if a.AccessorID == "" {

		a.AccessorID = uuid.Generate()
		a.SecretID = uuid.Generate()
		a.CreateTime = time.Now().UTC()

		// If the user has not set the expiration time, but has provided a TTL, we
		// calculate and populate the former filed.
		if a.ExpirationTime == nil && a.ExpirationTTL != 0 {
			a.ExpirationTime = pointer.Of(a.CreateTime.Add(a.ExpirationTTL))
		}
	}
}

// Validate is used to check a token for reasonableness
func (a *ACLToken) Validate(minTTL, maxTTL time.Duration, existing *ACLToken) error {
	var mErr multierror.Error

	// The human friendly name of an ACL token cannot exceed 256 characters.
	if len(a.Name) > maxTokenNameLength {
		mErr.Errors = append(mErr.Errors, errors.New("token name too long"))
	}

	// The type of an ACL token must be set. An ACL token of type client must
	// have associated policies or roles, whereas a management token cannot be
	// associated with policies.
	switch a.Type {
	case ACLClientToken:
		if len(a.Policies) == 0 && len(a.Roles) == 0 {
			mErr.Errors = append(mErr.Errors, errors.New("client token missing policies or roles"))
		}
	case ACLManagementToken:
		if len(a.Policies) != 0 || len(a.Roles) != 0 {
			mErr.Errors = append(mErr.Errors, errors.New("management token cannot be associated with policies or roles"))
		}
	default:
		mErr.Errors = append(mErr.Errors, errors.New("token type must be client or management"))
	}

	// There are different validation rules depending on whether the ACL token
	// is being created or updated.
	switch existing {
	case nil:
		if a.ExpirationTTL < 0 {
			mErr.Errors = append(mErr.Errors,
				fmt.Errorf("token expiration TTL '%s' should not be negative", a.ExpirationTTL))
		}

		if a.ExpirationTime != nil && !a.ExpirationTime.IsZero() {

			if a.CreateTime.After(*a.ExpirationTime) {
				mErr.Errors = append(mErr.Errors, errors.New("expiration time cannot be before create time"))
			}

			// Create a time duration which details the time-til-expiry, so we can
			// check this against the regions max and min values.
			expiresIn := a.ExpirationTime.Sub(a.CreateTime)
			if expiresIn > maxTTL {
				mErr.Errors = append(mErr.Errors,
					fmt.Errorf("expiration time cannot be more than %s in the future (was %s)",
						maxTTL, expiresIn))

			} else if expiresIn < minTTL {
				mErr.Errors = append(mErr.Errors,
					fmt.Errorf("expiration time cannot be less than %s in the future (was %s)",
						minTTL, expiresIn))
			}
		}
	default:
		if existing.Global != a.Global {
			mErr.Errors = append(mErr.Errors, errors.New("cannot toggle global mode"))
		}
		if existing.ExpirationTTL != a.ExpirationTTL {
			mErr.Errors = append(mErr.Errors, errors.New("cannot update expiration TTL"))
		}
		if existing.ExpirationTime != a.ExpirationTime {
			mErr.Errors = append(mErr.Errors, errors.New("cannot update expiration time"))
		}
	}

	return mErr.ErrorOrNil()
}

// HasExpirationTime checks whether the ACL token has an expiration time value
// set.
func (a *ACLToken) HasExpirationTime() bool {
	if a == nil || a.ExpirationTime == nil {
		return false
	}
	return !a.ExpirationTime.IsZero()
}

// IsExpired compares the ACLToken.ExpirationTime against the passed t to
// identify whether the token is considered expired. The function can be called
// without checking whether the ACL token has an expiry time.
func (a *ACLToken) IsExpired(t time.Time) bool {

	// Check the token has an expiration time before potentially modifying the
	// supplied time. This allows us to avoid extra work, if it isn't needed.
	if !a.HasExpirationTime() {
		return false
	}

	// Check and ensure the time location is set to UTC. This is vital for
	// consistency with multi-region global tokens.
	if t.Location() != time.UTC {
		t = t.UTC()
	}

	return a.ExpirationTime.Before(t) || t.IsZero()
}

// HasRoles checks if a given set of role IDs are assigned to the ACL token. It
// does not account for management tokens, therefore it is the responsibility
// of the caller to perform this check, if required.
func (a *ACLToken) HasRoles(roleIDs []string) bool {

	// Generate a set of role IDs that the token is assigned.
	roleSet := set.FromFunc(a.Roles, func(roleLink *ACLTokenRoleLink) string { return roleLink.ID })

	// Iterate the role IDs within the request and check whether these are
	// present within the token assignment.
	for _, roleID := range roleIDs {
		if !roleSet.Contains(roleID) {
			return false
		}
	}
	return true
}

// ACLRole is an abstraction for the ACL system which allows the grouping of
// ACL policies into a single object. ACL tokens can be created and linked to
// a role; the token then inherits all the permissions granted by the policies.
type ACLRole struct {

	// ID is an internally generated UUID for this role and is controlled by
	// Nomad.
	ID string

	// Name is unique across the entire set of federated clusters and is
	// supplied by the operator on role creation. The name can be modified by
	// updating the role and including the Nomad generated ID. This update will
	// not affect tokens created and linked to this role. This is a required
	// field.
	Name string

	// Description is a human-readable, operator set description that can
	// provide additional context about the role. This is an operational field.
	Description string

	// Policies is an array of ACL policy links. Although currently policies
	// can only be linked using their name, in the future we will want to add
	// IDs also and thus allow operators to specify either a name, an ID, or
	// both.
	Policies []*ACLRolePolicyLink

	// Hash is the hashed value of the role and is generated using all fields
	// above this point.
	Hash []byte

	CreateIndex uint64
	ModifyIndex uint64
}

// ACLRolePolicyLink is used to link a policy to an ACL role. We use a struct
// rather than a list of strings as in the future we will want to add IDs to
// policies and then link via these.
type ACLRolePolicyLink struct {

	// Name is the ACLPolicy.Name value which will be linked to the ACL role.
	Name string
}

// SetHash is used to compute and set the hash of the ACL role. This should be
// called every and each time a user specified field on the role is changed
// before updating the Nomad state store.
func (a *ACLRole) SetHash() []byte {

	// Initialize a 256bit Blake2 hash (32 bytes).
	hash, err := blake2b.New256(nil)
	if err != nil {
		panic(err)
	}

	// Write all the user set fields.
	_, _ = hash.Write([]byte(a.Name))
	_, _ = hash.Write([]byte(a.Description))

	for _, policyLink := range a.Policies {
		_, _ = hash.Write([]byte(policyLink.Name))
	}

	// Finalize the hash.
	hashVal := hash.Sum(nil)

	// Set and return the hash.
	a.Hash = hashVal
	return hashVal
}

// Validate ensure the ACL role contains valid information which meets Nomad's
// internal requirements. This does not include any state calls, such as
// ensuring the linked policies exist.
func (a *ACLRole) Validate() error {

	var mErr multierror.Error

	if !validACLRoleName.MatchString(a.Name) {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("invalid name '%s'", a.Name))
	}

	if len(a.Description) > maxACLRoleDescriptionLength {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("description longer than %d", maxACLRoleDescriptionLength))
	}

	if len(a.Policies) < 1 {
		mErr.Errors = append(mErr.Errors, errors.New("at least one policy should be specified"))
	}

	return mErr.ErrorOrNil()
}

// Canonicalize performs basic canonicalization on the ACL role object. It is
// important for callers to understand certain fields such as ID are set if it
// is empty, so copies should be taken if needed before calling this function.
func (a *ACLRole) Canonicalize() {
	if a.ID == "" {
		a.ID = uuid.Generate()
	}
}

// Equal performs an equality check on the two service registrations. It
// handles nil objects.
func (a *ACLRole) Equal(o *ACLRole) bool {
	if a == nil || o == nil {
		return a == o
	}
	if len(a.Hash) == 0 {
		a.SetHash()
	}
	if len(o.Hash) == 0 {
		o.SetHash()
	}
	return bytes.Equal(a.Hash, o.Hash)
}

// Copy creates a deep copy of the ACL role. This copy can then be safely
// modified. It handles nil objects.
func (a *ACLRole) Copy() *ACLRole {
	if a == nil {
		return nil
	}

	c := new(ACLRole)
	*c = *a

	c.Policies = slices.Clone(a.Policies)
	c.Hash = slices.Clone(a.Hash)

	return c
}

// Stub converts the ACLRole object into a ACLRoleListStub object.
func (a *ACLRole) Stub() *ACLRoleListStub {
	return &ACLRoleListStub{
		ID:          a.ID,
		Name:        a.Name,
		Description: a.Description,
		Policies:    a.Policies,
		Hash:        a.Hash,
		CreateIndex: a.CreateIndex,
		ModifyIndex: a.ModifyIndex,
	}
}

// ACLRoleListStub is the stub object returned when performing a listing of ACL
// roles. While it might not currently be different to the full response
// object, it allows us to future-proof the RPC in the event the ACLRole object
// grows over time.
type ACLRoleListStub struct {

	// ID is an internally generated UUID for this role and is controlled by
	// Nomad.
	ID string

	// Name is unique across the entire set of federated clusters and is
	// supplied by the operator on role creation. The name can be modified by
	// updating the role and including the Nomad generated ID. This update will
	// not affect tokens created and linked to this role. This is a required
	// field.
	Name string

	// Description is a human-readable, operator set description that can
	// provide additional context about the role. This is an operational field.
	Description string

	// Policies is an array of ACL policy links. Although currently policies
	// can only be linked using their name, in the future we will want to add
	// IDs also and thus allow operators to specify either a name, an ID, or
	// both.
	Policies []*ACLRolePolicyLink

	// Hash is the hashed value of the role and is generated using all fields
	// above this point.
	Hash []byte

	CreateIndex uint64
	ModifyIndex uint64
}

// ACLRolesUpsertRequest is the request object used to upsert one or more ACL
// roles.
type ACLRolesUpsertRequest struct {
	ACLRoles []*ACLRole

	// AllowMissingPolicies skips the ACL Role policy link verification and is
	// used by the replication process. The replication cannot ensure policies
	// are present before ACL Roles are replicated.
	AllowMissingPolicies bool

	WriteRequest
}

// ACLRolesUpsertResponse is the response object when one or more ACL roles
// have been successfully upserted into state.
type ACLRolesUpsertResponse struct {
	ACLRoles []*ACLRole
	WriteMeta
}

// ACLRolesDeleteByIDRequest is the request object to delete one or more ACL
// roles using the role ID.
type ACLRolesDeleteByIDRequest struct {
	ACLRoleIDs []string
	WriteRequest
}

// ACLRolesDeleteByIDResponse is the response object when performing a deletion
// of one or more ACL roles using the role ID.
type ACLRolesDeleteByIDResponse struct {
	WriteMeta
}

// ACLRolesListRequest is the request object when performing ACL role listings.
type ACLRolesListRequest struct {
	QueryOptions
}

// ACLRolesListResponse is the response object when performing ACL role
// listings.
type ACLRolesListResponse struct {
	ACLRoles []*ACLRoleListStub
	QueryMeta
}

// ACLRolesByIDRequest is the request object when performing a lookup of
// multiple roles by the ID.
type ACLRolesByIDRequest struct {
	ACLRoleIDs []string
	QueryOptions
}

// ACLRolesByIDResponse is the response object when performing a lookup of
// multiple roles by their IDs.
type ACLRolesByIDResponse struct {
	ACLRoles map[string]*ACLRole
	QueryMeta
}

// ACLRoleByIDRequest is the request object to perform a lookup of an ACL
// role using a specific ID.
type ACLRoleByIDRequest struct {
	RoleID string
	QueryOptions
}

// ACLRoleByIDResponse is the response object when performing a lookup of an
// ACL role matching a specific ID.
type ACLRoleByIDResponse struct {
	ACLRole *ACLRole
	QueryMeta
}

// ACLRoleByNameRequest is the request object to perform a lookup of an ACL
// role using a specific name.
type ACLRoleByNameRequest struct {
	RoleName string
	QueryOptions
}

// ACLRoleByNameResponse is the response object when performing a lookup of an
// ACL role matching a specific name.
type ACLRoleByNameResponse struct {
	ACLRole *ACLRole
	QueryMeta
}

// ACLAuthMethod is used to capture the properties of an authentication method
// used for single sing-on
type ACLAuthMethod struct {
	Name          string
	Type          string
	TokenLocality string // is the token valid locally or globally?
	MaxTokenTTL   time.Duration
	Default       bool
	Config        *ACLAuthMethodConfig

	Hash []byte

	CreateTime  time.Time
	ModifyTime  time.Time
	CreateIndex uint64
	ModifyIndex uint64
}

// SetHash is used to compute and set the hash of the ACL auth method. This
// should be called every and each time a user specified field on the method is
// changed before updating the Nomad state store.
func (a *ACLAuthMethod) SetHash() []byte {

	// Initialize a 256bit Blake2 hash (32 bytes).
	hash, err := blake2b.New256(nil)
	if err != nil {
		panic(err)
	}

	_, _ = hash.Write([]byte(a.Name))
	_, _ = hash.Write([]byte(a.Type))
	_, _ = hash.Write([]byte(a.TokenLocality))
	_, _ = hash.Write([]byte(a.MaxTokenTTL.String()))
	_, _ = hash.Write([]byte(strconv.FormatBool(a.Default)))

	if a.Config != nil {
		_, _ = hash.Write([]byte(a.Config.OIDCDiscoveryURL))
		_, _ = hash.Write([]byte(a.Config.OIDCClientID))
		_, _ = hash.Write([]byte(a.Config.OIDCClientSecret))
		for _, ba := range a.Config.BoundAudiences {
			_, _ = hash.Write([]byte(ba))
		}
		for _, uri := range a.Config.AllowedRedirectURIs {
			_, _ = hash.Write([]byte(uri))
		}
		for _, pem := range a.Config.DiscoveryCaPem {
			_, _ = hash.Write([]byte(pem))
		}
		for _, sa := range a.Config.SigningAlgs {
			_, _ = hash.Write([]byte(sa))
		}
		for k, v := range a.Config.ClaimMappings {
			_, _ = hash.Write([]byte(k))
			_, _ = hash.Write([]byte(v))
		}
		for k, v := range a.Config.ListClaimMappings {
			_, _ = hash.Write([]byte(k))
			_, _ = hash.Write([]byte(v))
		}
	}

	// Finalize the hash.
	hashVal := hash.Sum(nil)

	// Set and return the hash.
	a.Hash = hashVal
	return hashVal
}

// MarshalJSON implements the json.Marshaler interface and allows
// ACLAuthMethod.MaxTokenTTL to be marshaled correctly.
func (a *ACLAuthMethod) MarshalJSON() ([]byte, error) {
	type Alias ACLAuthMethod
	exported := &struct {
		MaxTokenTTL string
		*Alias
	}{
		MaxTokenTTL: a.MaxTokenTTL.String(),
		Alias:       (*Alias)(a),
	}
	if a.MaxTokenTTL == 0 {
		exported.MaxTokenTTL = ""
	}
	return json.Marshal(exported)
}

// UnmarshalJSON implements the json.Unmarshaler interface and allows
// ACLAuthMethod.MaxTokenTTL to be unmarshalled correctly.
func (a *ACLAuthMethod) UnmarshalJSON(data []byte) (err error) {
	type Alias ACLAuthMethod
	aux := &struct {
		MaxTokenTTL interface{}
		*Alias
	}{
		Alias: (*Alias)(a),
	}
	if err = json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.MaxTokenTTL != nil {
		switch v := aux.MaxTokenTTL.(type) {
		case string:
			if a.MaxTokenTTL, err = time.ParseDuration(v); err != nil {
				return err
			}
		case float64:
			a.MaxTokenTTL = time.Duration(v)
		}
	}
	return nil
}

func (a *ACLAuthMethod) Stub() *ACLAuthMethodStub {
	return &ACLAuthMethodStub{
		Name:        a.Name,
		Type:        a.Type,
		Default:     a.Default,
		Hash:        a.Hash,
		CreateIndex: a.CreateIndex,
		ModifyIndex: a.ModifyIndex,
	}
}

func (a *ACLAuthMethod) Equal(other *ACLAuthMethod) bool {
	if a == nil || other == nil {
		return a == other
	}
	if len(a.Hash) == 0 {
		a.SetHash()
	}
	if len(other.Hash) == 0 {
		other.SetHash()
	}
	return bytes.Equal(a.Hash, other.Hash)

}

// Copy creates a deep copy of the ACL auth method. This copy can then be safely
// modified. It handles nil objects.
func (a *ACLAuthMethod) Copy() *ACLAuthMethod {
	if a == nil {
		return nil
	}

	c := new(ACLAuthMethod)
	*c = *a

	c.Hash = slices.Clone(a.Hash)
	c.Config = a.Config.Copy()

	return c
}

// Canonicalize performs basic canonicalization on the ACL auth method object.
func (a *ACLAuthMethod) Canonicalize() {
	t := time.Now().UTC()

	if a.CreateTime.IsZero() {
		a.CreateTime = t
	}
	a.ModifyTime = t
}

// Validate returns an error is the ACLAuthMethod is invalid.
//
// TODO revisit possible other validity conditions in the future
func (a *ACLAuthMethod) Validate(minTTL, maxTTL time.Duration) error {
	var mErr multierror.Error

	if !validACLAuthMethod.MatchString(a.Name) {
		mErr.Errors = append(mErr.Errors, fmt.Errorf("invalid name '%s'", a.Name))
	}

	if !slices.Contains([]string{"local", "global"}, a.TokenLocality) {
		mErr.Errors = append(
			mErr.Errors, fmt.Errorf("invalid token locality '%s'", a.TokenLocality))
	}

	if a.Type != "OIDC" {
		mErr.Errors = append(
			mErr.Errors, fmt.Errorf("invalid token type '%s'", a.Type))
	}

	if minTTL > a.MaxTokenTTL || a.MaxTokenTTL > maxTTL {
		mErr.Errors = append(mErr.Errors, fmt.Errorf(
			"invalid MaxTokenTTL value '%s' (should be between %s and %s)",
			a.MaxTokenTTL.String(), minTTL.String(), maxTTL.String()))
	}

	return mErr.ErrorOrNil()
}

// ACLAuthMethodConfig is used to store configuration of an auth method
type ACLAuthMethodConfig struct {
	OIDCDiscoveryURL    string
	OIDCClientID        string
	OIDCClientSecret    string
	BoundAudiences      []string
	AllowedRedirectURIs []string
	DiscoveryCaPem      []string
	SigningAlgs         []string
	ClaimMappings       map[string]string
	ListClaimMappings   map[string]string
}

func (a *ACLAuthMethodConfig) Copy() *ACLAuthMethodConfig {
	if a == nil {
		return nil
	}

	c := new(ACLAuthMethodConfig)
	*c = *a

	c.BoundAudiences = slices.Clone(a.BoundAudiences)
	c.AllowedRedirectURIs = slices.Clone(a.AllowedRedirectURIs)
	c.DiscoveryCaPem = slices.Clone(a.DiscoveryCaPem)
	c.SigningAlgs = slices.Clone(a.SigningAlgs)

	return c
}

// ACLAuthMethodStub is used for listing ACL auth methods
type ACLAuthMethodStub struct {
	Name    string
	Type    string
	Default bool

	// Hash is the hashed value of the auth-method and is generated using all
	// fields from the full object except the create and modify times and
	// indexes.
	Hash []byte

	CreateIndex uint64
	ModifyIndex uint64
}

// ACLAuthMethodListRequest is used to list auth methods
type ACLAuthMethodListRequest struct {
	QueryOptions
}

// ACLAuthMethodListResponse is used to list auth methods
type ACLAuthMethodListResponse struct {
	AuthMethods []*ACLAuthMethodStub
	QueryMeta
}

// ACLAuthMethodGetRequest is used to query a specific auth method
type ACLAuthMethodGetRequest struct {
	MethodName string
	QueryOptions
}

// ACLAuthMethodGetResponse is used to return a single auth method
type ACLAuthMethodGetResponse struct {
	AuthMethod *ACLAuthMethod
	QueryMeta
}

// ACLAuthMethodsGetRequest is used to query a set of auth methods
type ACLAuthMethodsGetRequest struct {
	Names []string
	QueryOptions
}

// ACLAuthMethodsGetResponse is used to return a set of auth methods
type ACLAuthMethodsGetResponse struct {
	AuthMethods map[string]*ACLAuthMethod
	QueryMeta
}

// ACLAuthMethodUpsertRequest is used to upsert a set of auth methods
type ACLAuthMethodUpsertRequest struct {
	AuthMethods []*ACLAuthMethod
	WriteRequest
}

// ACLAuthMethodUpsertResponse is a response of the upsert ACL auth methods
// operation
type ACLAuthMethodUpsertResponse struct {
	AuthMethods []*ACLAuthMethod
	WriteMeta
}

// ACLAuthMethodDeleteRequest is used to delete a set of auth methods by their
// name
type ACLAuthMethodDeleteRequest struct {
	Names []string
	WriteRequest
}

// ACLAuthMethodDeleteResponse is a response of the delete ACL auth methods
// operation
type ACLAuthMethodDeleteResponse struct {
	WriteMeta
}

type ACLWhoAmIResponse struct {
	Identity *AuthenticatedIdentity
	QueryMeta
}
