// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package nomad

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v3"
	"github.com/go-jose/go-jose/v3/jwt"
	log "github.com/hashicorp/go-hclog"
	kms "github.com/hashicorp/go-kms-wrapping/v2"
	wrapping "github.com/hashicorp/go-kms-wrapping/v2"
	"github.com/hashicorp/go-kms-wrapping/v2/aead"
	"github.com/hashicorp/go-kms-wrapping/wrappers/awskms/v2"
	"github.com/hashicorp/go-kms-wrapping/wrappers/azurekeyvault/v2"
	"github.com/hashicorp/go-kms-wrapping/wrappers/gcpckms/v2"
	"github.com/hashicorp/go-kms-wrapping/wrappers/transit/v2"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/crypto"
	"github.com/hashicorp/nomad/helper/joseutil"
	"github.com/hashicorp/nomad/nomad/structs"
	"golang.org/x/time/rate"
)

const nomadKeystoreExtension = ".nks.json"

type claimSigner interface {
	SignClaims(*structs.IdentityClaims) (string, string, error)
}

var _ claimSigner = &Encrypter{}

// Encrypter is the keyring for encrypting variables and signing workload
// identities.
type Encrypter struct {
	srv             *Server
	providerConfigs map[string]*structs.KEKProviderConfig
	keystorePath    string

	// issuer is the OIDC Issuer to use for workload identities if configured
	issuer string

	keyring map[string]*keyset
	lock    sync.RWMutex
}

// keyset contains the key material for variable encryption and workload
// identity signing. As keysets are rotated they are identified by the RootKey
// KeyID although the public key IDs are published with a type prefix to
// disambiguate which signing algorithm to use.
type keyset struct {
	rootKey           *structs.RootKey
	cipher            cipher.AEAD
	eddsaPrivateKey   ed25519.PrivateKey
	rsaPrivateKey     *rsa.PrivateKey
	rsaPKCS1PublicKey []byte // PKCS #1 DER encoded public key for JWKS
}

// NewEncrypter loads or creates a new local keystore and returns an
// encryption keyring with the keys it finds.
func NewEncrypter(srv *Server, keystorePath string) (*Encrypter, error) {

	encrypter := &Encrypter{
		srv:          srv,
		keystorePath: keystorePath,
		keyring:      make(map[string]*keyset),
		issuer:       srv.GetConfig().OIDCIssuer,
	}
	for _, provider := range srv.config.KEKProviderConfigs {
		encrypter.providerConfigs[provider.Provider+provider.Name] = provider
	}

	err := encrypter.loadKeystore()
	if err != nil {
		return nil, err
	}
	return encrypter, nil
}

func (e *Encrypter) loadKeystore() error {

	if err := os.MkdirAll(e.keystorePath, 0o700); err != nil {
		return err
	}

	return filepath.Walk(e.keystorePath, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("could not read path %s from keystore: %v", path, err)
		}

		// skip over subdirectories and non-key files; they shouldn't
		// be here but there's no reason to fail startup for it if the
		// administrator has left something there
		if path != e.keystorePath && info.IsDir() {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(path, nomadKeystoreExtension) {
			return nil
		}
		id := strings.TrimSuffix(filepath.Base(path), nomadKeystoreExtension)
		if !helper.IsUUID(id) {
			return nil
		}

		key, err := e.loadKeyFromStore(path)
		if err != nil {
			return fmt.Errorf("could not load key file %s from keystore: %w", path, err)
		}
		if key.Meta.KeyID != id {
			return fmt.Errorf("root key ID %s must match key file %s", key.Meta.KeyID, path)
		}

		err = e.addCipher(key)
		if err != nil {
			return fmt.Errorf("could not add key file %s to keystore: %w", path, err)
		}
		return nil
	})
}

// Encrypt encrypts the clear data with the cipher for the current
// root key, and returns the cipher text (including the nonce), and
// the key ID used to encrypt it
func (e *Encrypter) Encrypt(cleartext []byte) ([]byte, string, error) {

	keyset, err := e.activeKeySet()
	if err != nil {
		return nil, "", err
	}

	nonce, err := crypto.Bytes(keyset.cipher.NonceSize())
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate key wrapper nonce: %v", err)
	}

	keyID := keyset.rootKey.Meta.KeyID
	additional := []byte(keyID) // include the keyID in the signature inputs

	// we use the nonce as the dst buffer so that the ciphertext is
	// appended to that buffer and we always keep the nonce and
	// ciphertext together, and so that we're not tempted to reuse
	// the cleartext buffer which the caller still owns
	ciphertext := keyset.cipher.Seal(nonce, nonce, cleartext, additional)
	return ciphertext, keyID, nil
}

// Decrypt takes an encrypted buffer and then root key ID. It extracts
// the nonce, decrypts the content, and returns the cleartext data.
func (e *Encrypter) Decrypt(ciphertext []byte, keyID string) ([]byte, error) {
	e.lock.RLock()
	defer e.lock.RUnlock()

	keyset, err := e.keysetByIDLocked(keyID)
	if err != nil {
		return nil, err
	}

	nonceSize := keyset.cipher.NonceSize()
	nonce := ciphertext[:nonceSize] // nonce was stored alongside ciphertext
	additional := []byte(keyID)     // keyID was included in the signature inputs

	return keyset.cipher.Open(nil, nonce, ciphertext[nonceSize:], additional)
}

// keyIDHeader is the JWT header for the Nomad Key ID used to sign the
// claim. This name matches the common industry practice for this
// header name.
const keyIDHeader = "kid"

// SignClaims signs the identity claim for the task and returns an encoded JWT
// (including both the claim and its signature) and the key ID of the key used
// to sign it, or an error.
//
// SignClaims adds the Issuer claim prior to signing.
func (e *Encrypter) SignClaims(claims *structs.IdentityClaims) (string, string, error) {

	// If a key is rotated immediately following a leader election, plans that
	// are in-flight may get signed before the new leader has the key. Allow for
	// a short timeout-and-retry to avoid rejecting plans
	keyset, err := e.activeKeySet()
	if err != nil {
		ctx, cancel := context.WithTimeout(e.srv.shutdownCtx, 5*time.Second)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return "", "", err
			default:
				time.Sleep(50 * time.Millisecond)
				keyset, err = e.activeKeySet()
				if keyset != nil {
					break
				}
			}
		}
	}

	// Add Issuer claim from server configuration
	if e.issuer != "" {
		claims.Issuer = e.issuer
	}

	opts := (&jose.SignerOptions{}).WithHeader("kid", keyset.rootKey.Meta.KeyID).WithType("JWT")

	var sig jose.Signer
	if keyset.rsaPrivateKey != nil {
		// If an RSA key has been created prefer it as it is more widely compatible
		sig, err = jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: keyset.rsaPrivateKey}, opts)
		if err != nil {
			return "", "", err
		}
	} else {
		// No RSA key has been created, fallback to ed25519 which always exists
		sig, err = jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: keyset.eddsaPrivateKey}, opts)
		if err != nil {
			return "", "", err
		}
	}

	raw, err := jwt.Signed(sig).Claims(claims).CompactSerialize()
	if err != nil {
		return "", "", err
	}

	return raw, keyset.rootKey.Meta.KeyID, nil
}

// VerifyClaim accepts a previously-signed encoded claim and validates
// it before returning the claim
func (e *Encrypter) VerifyClaim(tokenString string) (*structs.IdentityClaims, error) {

	token, err := jwt.ParseSigned(tokenString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse signed token: %w", err)
	}

	// Find the Key ID
	keyID, err := joseutil.KeyID(token)
	if err != nil {
		return nil, err
	}

	// Find the Key
	pubKey, err := e.GetPublicKey(keyID)
	if err != nil {
		return nil, err
	}

	typedPubKey, err := pubKey.GetPublicKey()
	if err != nil {
		return nil, err
	}

	// Validate the claims.
	claims := &structs.IdentityClaims{}
	if err := token.Claims(typedPubKey, claims); err != nil {
		return nil, fmt.Errorf("invalid signature: %w", err)
	}

	//COMPAT Until we can guarantee there are no pre-1.7 JWTs in use we can only
	//       validate the signature and have no further expectations of the
	//       claims.
	expect := jwt.Expected{}
	if err := claims.Validate(expect); err != nil {
		return nil, fmt.Errorf("invalid claims: %w", err)
	}

	return claims, nil
}

// AddKey stores the key in the keystore and creates a new cipher for it.
func (e *Encrypter) AddKey(rootKey *structs.RootKey) error {

	// note: we don't lock the keyring here but inside addCipher
	// instead, so that we're not holding the lock while performing
	// local disk writes
	if err := e.addCipher(rootKey); err != nil {
		return err
	}
	if err := e.saveKeyToStore(rootKey); err != nil {
		return err
	}
	return nil
}

// addCipher stores the key in the keyring and creates a new cipher for it.
func (e *Encrypter) addCipher(rootKey *structs.RootKey) error {

	if rootKey == nil || rootKey.Meta == nil {
		return fmt.Errorf("missing metadata")
	}
	var aead cipher.AEAD

	switch rootKey.Meta.Algorithm {
	case structs.EncryptionAlgorithmAES256GCM:
		block, err := aes.NewCipher(rootKey.Key)
		if err != nil {
			return fmt.Errorf("could not create cipher: %v", err)
		}
		aead, err = cipher.NewGCM(block)
		if err != nil {
			return fmt.Errorf("could not create cipher: %v", err)
		}
	default:
		return fmt.Errorf("invalid algorithm %s", rootKey.Meta.Algorithm)
	}

	ed25519Key := ed25519.NewKeyFromSeed(rootKey.Key)

	ks := keyset{
		rootKey:         rootKey,
		cipher:          aead,
		eddsaPrivateKey: ed25519Key,
	}

	// Unmarshal RSAKey for Workload Identity JWT signing if one exists. Prior to
	// 1.7 only the ed25519 key was used.
	if len(rootKey.RSAKey) > 0 {
		rsaKey, err := x509.ParsePKCS1PrivateKey(rootKey.RSAKey)
		if err != nil {
			return fmt.Errorf("error parsing rsa key: %w", err)
		}

		ks.rsaPrivateKey = rsaKey
		ks.rsaPKCS1PublicKey = x509.MarshalPKCS1PublicKey(&rsaKey.PublicKey)
	}

	e.lock.Lock()
	defer e.lock.Unlock()
	e.keyring[rootKey.Meta.KeyID] = &ks
	return nil
}

// GetKey retrieves the key material by ID from the keyring
func (e *Encrypter) GetKey(keyID string) ([]byte, []byte, error) {
	e.lock.RLock()
	defer e.lock.RUnlock()

	keyset, err := e.keysetByIDLocked(keyID)
	if err != nil {
		return nil, nil, err
	}
	return keyset.rootKey.Key, keyset.rootKey.RSAKey, nil
}

// activeKeySetLocked returns the keyset that belongs to the key marked as
// active in the state store (so that it's consistent with raft). The
// called must read-lock the keyring
func (e *Encrypter) activeKeySet() (*keyset, error) {
	store := e.srv.fsm.State()
	keyMeta, err := store.GetActiveRootKeyMeta(nil)
	if err != nil {
		return nil, err
	}
	if keyMeta == nil {
		return nil, fmt.Errorf("keyring has not been initialized yet")
	}
	e.lock.RLock()
	defer e.lock.RUnlock()
	return e.keysetByIDLocked(keyMeta.KeyID)
}

// keysetByIDLocked returns the keyset for the specified keyID. The
// caller must read-lock the keyring
func (e *Encrypter) keysetByIDLocked(keyID string) (*keyset, error) {
	keyset, ok := e.keyring[keyID]
	if !ok {
		return nil, fmt.Errorf("no such key %q in keyring", keyID)
	}
	return keyset, nil
}

// RemoveKey removes a key by ID from the keyring
func (e *Encrypter) RemoveKey(keyID string) error {
	e.lock.Lock()
	defer e.lock.Unlock()
	delete(e.keyring, keyID)
	return nil
}

func (e *Encrypter) encryptDEK(rootKey *structs.RootKey, provider *structs.KEKProvider) (*structs.KeyEncryptionKeyWrapper, error) {
	if provider == nil {
		panic("can't encrypt DEK without a provider")
	}
	var kek []byte
	var err error
	if provider.Provider == "aead" || provider.Provider == "" {
		kek, err = crypto.Bytes(32)
		if err != nil {
			return nil, fmt.Errorf("failed to generate key wrapper key: %w", err)
		}
	}
	wrapper, err := e.newKMSWrapper(provider.Provider, provider.Name, rootKey.Meta.KeyID, kek)
	rootBlob, err := wrapper.Encrypt(e.srv.shutdownCtx, rootKey.Key)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt root key: %w", err)
	}
	kekWrapper := &structs.KeyEncryptionKeyWrapper{
		Meta:                       rootKey.Meta,
		EncryptedDataEncryptionKey: rootBlob.Ciphertext,
		KeyEncryptionKey:           kek,
		Provider:                   provider.Provider,
		ProviderName:               provider.Name,
	}

	// Only keysets created after 1.7.0 will contain an RSA key.
	if len(rootKey.RSAKey) > 0 {
		rsaBlob, err := wrapper.Encrypt(e.srv.shutdownCtx, rootKey.RSAKey)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt rsa key: %w", err)
		}
		kekWrapper.EncryptedRSAKey = rsaBlob.Ciphertext
	}

	return kekWrapper, nil
}

// saveKeyToStore serializes a root key to the on-disk keystore.
func (e *Encrypter) saveKeyToStore(rootKey *structs.RootKey) error {

	for _, provider := range rootKey.Meta.KEKProviders {
		kekWrapper, err := e.encryptDEK(rootKey, provider)
		if err != nil {
			return err
		}

		buf, err := json.Marshal(kekWrapper)
		if err != nil {
			return err
		}

		extension := nomadKeystoreExtension
		if provider.Provider != "aead" {
			extension = "." + provider.Provider
			if provider.Name != "" {
				extension = extension + "." + provider.Name
			}
			extension = extension + nomadKeystoreExtension
		}

		path := filepath.Join(e.keystorePath, rootKey.Meta.KeyID+extension)
		err = os.WriteFile(path, buf, 0o600)
		if err != nil {
			return err
		}
	}

	return nil
}

// loadKeyFromStore deserializes a root key from disk.
func (e *Encrypter) loadKeyFromStore(path string) (*structs.RootKey, error) {

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	kekWrapper := &structs.KeyEncryptionKeyWrapper{}
	if err := json.Unmarshal(raw, kekWrapper); err != nil {
		return nil, err
	}

	meta := kekWrapper.Meta
	if err = meta.Validate(); err != nil {
		return nil, err
	}

	// the errors that bubble up from this library can be a bit opaque, so make
	// sure we wrap them with as much context as possible
	wrapper, err := e.newKMSWrapper(
		kekWrapper.Provider, kekWrapper.ProviderName,
		meta.KeyID, kekWrapper.KeyEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("unable to create key wrapper cipher: %w", err)
	}
	key, err := wrapper.Decrypt(e.srv.shutdownCtx, &kms.BlobInfo{
		Ciphertext: kekWrapper.EncryptedDataEncryptionKey,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to decrypt wrapped root key: %w", err)
	}

	// Decrypt RSAKey for Workload Identity JWT signing if one exists. Prior to
	// 1.7 an ed25519 key derived from the root key was used instead of an RSA
	// key.
	var rsaKey []byte
	if len(kekWrapper.EncryptedRSAKey) > 0 {
		rsaKey, err = wrapper.Decrypt(e.srv.shutdownCtx, &kms.BlobInfo{
			Ciphertext: kekWrapper.EncryptedRSAKey,
		})
		if err != nil {
			return nil, fmt.Errorf("unable to decrypt wrapped rsa key: %w", err)
		}
	}

	return &structs.RootKey{
		Meta:   meta,
		Key:    key,
		RSAKey: rsaKey,
	}, nil
}

// GetPublicKey returns the public signing key for the requested key id or an
// error if the key could not be found.
func (e *Encrypter) GetPublicKey(keyID string) (*structs.KeyringPublicKey, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	ks, err := e.keysetByIDLocked(keyID)
	if err != nil {
		return nil, err
	}

	pubKey := &structs.KeyringPublicKey{
		KeyID:      keyID,
		Use:        structs.PubKeyUseSig,
		CreateTime: ks.rootKey.Meta.CreateTime,
	}

	if ks.rsaPrivateKey != nil {
		pubKey.PublicKey = ks.rsaPKCS1PublicKey
		pubKey.Algorithm = structs.PubKeyAlgRS256
	} else {
		pubKey.PublicKey = ks.eddsaPrivateKey.Public().(ed25519.PublicKey)
		pubKey.Algorithm = structs.PubKeyAlgEdDSA
	}

	return pubKey, nil
}

// newKMSWrapper returns a go-kms-wrapping interface the caller can use to
// encrypt the RootKey with a key encryption key (KEK).
func (e *Encrypter) newKMSWrapper(provider, name string, keyID string, kek []byte) (kms.Wrapper, error) {
	var wrapper kms.Wrapper

	// TODO: should we bother to add OCI, AliCloud, Tencent, and Huawei?
	switch provider {
	case "awskms":
		wrapper = awskms.NewWrapper()
	case "azurekeyvault":
		wrapper = azurekeyvault.NewWrapper()
	case "gcpckms":
		wrapper = gcpckms.NewWrapper()
	case "transit":
		wrapper = transit.NewWrapper()
	default: // "aead"
		wrapper := aead.NewWrapper()
		wrapper.SetConfig(context.Background(),
			aead.WithAeadType(kms.AeadTypeAesGcm),
			aead.WithHashType(kms.HashTypeSha256),
			kms.WithKeyId(keyID),
		)
		err := wrapper.SetAesGcmKeyBytes(kek)
		if err != nil {
			return nil, err
		}
		return wrapper, nil
	}

	config, ok := e.providerConfigs[provider+name]
	if ok {
		_, err := wrapper.SetConfig(context.Background(), wrapping.WithConfigMap(config.Config))
		if err != nil {
			return nil, err
		}
	}
	return wrapper, nil
}

type KeyringReplicator struct {
	srv       *Server
	encrypter *Encrypter
	logger    log.Logger
	stopFn    context.CancelFunc
}

func NewKeyringReplicator(srv *Server, e *Encrypter) *KeyringReplicator {
	ctx, cancel := context.WithCancel(context.Background())
	repl := &KeyringReplicator{
		srv:       srv,
		encrypter: e,
		logger:    srv.logger.Named("keyring.replicator"),
		stopFn:    cancel,
	}
	go repl.run(ctx)
	return repl
}

// stop is provided for testing
func (krr *KeyringReplicator) stop() {
	krr.stopFn()
}

const keyringReplicationRate = 5

func (krr *KeyringReplicator) run(ctx context.Context) {
	krr.logger.Debug("starting encryption key replication")
	defer krr.logger.Debug("exiting key replication")

	limiter := rate.NewLimiter(keyringReplicationRate, keyringReplicationRate)

	for {
		select {
		case <-krr.srv.shutdownCtx.Done():
			return
		case <-ctx.Done():
			return
		default:
			err := limiter.Wait(ctx)
			if err != nil {
				continue // rate limit exceeded
			}

			store := krr.srv.fsm.State()
			iter, err := store.RootKeyMetas(nil)
			if err != nil {
				krr.logger.Error("failed to fetch keyring", "error", err)
				continue
			}
			for {
				raw := iter.Next()
				if raw == nil {
					break
				}

				keyMeta := raw.(*structs.RootKeyMeta)
				if key, _, err := krr.encrypter.GetKey(keyMeta.KeyID); err == nil && len(key) > 0 {
					// the key material is immutable so if we've already got it
					// we can move on to the next key
					continue
				}

				err := krr.replicateKey(ctx, keyMeta)
				if err != nil {
					// don't break the loop on an error, as we want to make sure
					// we've replicated any keys we can. the rate limiter will
					// prevent this case from sending excessive RPCs
					krr.logger.Error(err.Error(), "key", keyMeta.KeyID)
				}

			}
		}
	}

}

// replicateKey replicates a single key from peer servers that was present in
// the state store but missing from the keyring. Returns an error only if no
// peers have this key.
func (krr *KeyringReplicator) replicateKey(ctx context.Context, keyMeta *structs.RootKeyMeta) error {
	keyID := keyMeta.KeyID
	krr.logger.Debug("replicating new key", "id", keyID)

	getReq := &structs.KeyringGetRootKeyRequest{
		KeyID: keyID,
		QueryOptions: structs.QueryOptions{
			Region:        krr.srv.config.Region,
			MinQueryIndex: keyMeta.ModifyIndex - 1,
		},
	}
	getResp := &structs.KeyringGetRootKeyResponse{}
	err := krr.srv.RPC("Keyring.Get", getReq, getResp)

	if err != nil || getResp.Key == nil {
		// Key replication needs to tolerate leadership flapping. If a key is
		// rotated during a leadership transition, it's possible that the new
		// leader has not yet replicated the key from the old leader before the
		// transition. Ask all the other servers if they have it.
		krr.logger.Warn("failed to fetch key from current leader, trying peers",
			"key", keyID, "error", err)
		getReq.AllowStale = true
		for _, peer := range krr.getAllPeers() {
			err = krr.srv.forwardServer(peer, "Keyring.Get", getReq, getResp)
			if err == nil && getResp.Key != nil {
				break
			}
		}
		if getResp.Key == nil {
			krr.logger.Error("failed to fetch key from any peer",
				"key", keyID, "error", err)
			return fmt.Errorf("failed to fetch key from any peer: %v", err)
		}
	}

	err = krr.encrypter.AddKey(getResp.Key)
	if err != nil {
		return fmt.Errorf("failed to add key to keyring: %v", err)
	}

	krr.logger.Debug("added key", "key", keyID)
	return nil
}

// TODO: move this method into Server?
func (krr *KeyringReplicator) getAllPeers() []*serverParts {
	krr.srv.peerLock.RLock()
	defer krr.srv.peerLock.RUnlock()
	peers := make([]*serverParts, 0, len(krr.srv.localPeers))
	for _, peer := range krr.srv.localPeers {
		peers = append(peers, peer.Copy())
	}
	return peers
}
