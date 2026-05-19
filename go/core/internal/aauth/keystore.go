package aauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// IssuerSecretName is the default name of the Secret that persists the
// controller's AAuth issuer keypair across pod restarts.
const IssuerSecretName = "kagent-aauth-issuer"

// Secret data keys.
const (
	secretKeyPrivatePEM = "ed25519-private.pem"
	secretKeyKid        = "kid"
)

// LoadOrGenerateIssuerKey returns the controller's persistent Ed25519 issuer key.
//
// On the first call (Secret does not exist), it generates a fresh keypair, writes
// it to a Secret in the given namespace, and returns it. On subsequent calls
// (controller restarts), it loads the existing keypair from the Secret so the
// JWKS published at /.well-known/jwks.json stays stable across restarts —
// outstanding aa-agent+jwt tokens minted before the restart remain verifiable.
//
// Concurrency: if two replicas race to create the Secret, the loser (409
// AlreadyExists) re-reads and adopts whatever the winner wrote.
//
// The rest.Config typically comes from mgr.GetConfig(). We use an uncached
// client because this runs before mgr.Start(), when the controller-runtime
// cache isn't ready.
func LoadOrGenerateIssuerKey(
	ctx context.Context,
	cfg *rest.Config,
	namespace string,
	secretName string,
) (ed25519.PrivateKey, ed25519.PublicKey, string, error) {
	if cfg == nil {
		return nil, nil, "", fmt.Errorf("aauth: rest.Config is required")
	}
	if namespace == "" {
		return nil, nil, "", fmt.Errorf("aauth: namespace is required")
	}
	if secretName == "" {
		secretName = IssuerSecretName
	}

	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, nil, "", fmt.Errorf("aauth: build k8s client: %w", err)
	}

	key := client.ObjectKey{Namespace: namespace, Name: secretName}

	// Fast path: Secret already exists.
	priv, pub, kid, found, err := tryLoadIssuerSecret(ctx, c, key)
	if err != nil {
		return nil, nil, "", err
	}
	if found {
		return priv, pub, kid, nil
	}

	// Slow path: generate and write.
	priv, pub, err = generateEd25519()
	if err != nil {
		return nil, nil, "", err
	}
	pemBytes, err := encodeEd25519PrivatePEM(priv)
	if err != nil {
		return nil, nil, "", err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kagent-controller",
				"app.kubernetes.io/component":  "aauth-issuer",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			secretKeyPrivatePEM: pemBytes,
			secretKeyKid:        []byte(DefaultKid),
		},
	}

	if err := c.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another replica won the race. Re-read.
			priv, pub, kid, found, err := tryLoadIssuerSecret(ctx, c, key)
			if err != nil {
				return nil, nil, "", err
			}
			if !found {
				return nil, nil, "", fmt.Errorf("aauth: Secret %s/%s vanished after AlreadyExists", namespace, secretName)
			}
			return priv, pub, kid, nil
		}
		return nil, nil, "", fmt.Errorf("aauth: create Secret %s/%s: %w", namespace, secretName, err)
	}

	return priv, pub, DefaultKid, nil
}

func tryLoadIssuerSecret(
	ctx context.Context,
	c client.Client,
	key client.ObjectKey,
) (ed25519.PrivateKey, ed25519.PublicKey, string, bool, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, "", false, nil
		}
		return nil, nil, "", false, fmt.Errorf("aauth: get Secret %s/%s: %w", key.Namespace, key.Name, err)
	}

	pemBytes, ok := secret.Data[secretKeyPrivatePEM]
	if !ok || len(pemBytes) == 0 {
		return nil, nil, "", false, fmt.Errorf("aauth: Secret %s/%s missing %q", key.Namespace, key.Name, secretKeyPrivatePEM)
	}

	priv, err := decodeEd25519PrivatePEM(pemBytes)
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("aauth: Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, nil, "", false, fmt.Errorf("aauth: Secret %s/%s: private key did not yield an Ed25519 public key", key.Namespace, key.Name)
	}

	kid := string(secret.Data[secretKeyKid])
	if kid == "" {
		kid = DefaultKid
	}

	return priv, pub, kid, true, nil
}

func generateEd25519() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("aauth: generate ed25519 key: %w", err)
	}
	return priv, pub, nil
}

func encodeEd25519PrivatePEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("aauth: marshal PKCS8 private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	}), nil
}

func decodeEd25519PrivatePEM(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found in private key data")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS8 private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("expected Ed25519 private key, got %T", key)
	}
	return priv, nil
}
