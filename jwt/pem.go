package jwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// ParsePrivateKeyPEM reads a PEM-encoded private key: PKCS#8 ("PRIVATE KEY", any
// of RSA, ECDSA or Ed25519), PKCS#1 ("RSA PRIVATE KEY") or SEC 1 ("EC PRIVATE
// KEY"). Encrypted keys are not supported. The result goes in Key.Private.
func ParsePrivateKeyPEM(data []byte) (crypto.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%w: not PEM data", ErrConfig)
	}
	if strings.Contains(block.Headers["Proc-Type"], "ENCRYPTED") || block.Type == "ENCRYPTED PRIVATE KEY" {
		return nil, fmt.Errorf("%w: encrypted PEM keys are not supported", ErrConfig)
	}
	switch block.Type {
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid PKCS#8 private key", ErrConfig)
		}
		return k, nil
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid PKCS#1 private key", ErrConfig)
		}
		return k, nil
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid EC private key", ErrConfig)
		}
		return k, nil
	}
	return nil, fmt.Errorf("%w: unsupported PEM block %q", ErrConfig, block.Type)
}

// ParsePublicKeyPEM reads a PEM-encoded public key ("PUBLIC KEY", PKIX; or
// "RSA PUBLIC KEY", PKCS#1) or the public key inside a "CERTIFICATE". The
// certificate's validity and chain are not checked. The result goes in
// Key.Public.
func ParsePublicKeyPEM(data []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%w: not PEM data", ErrConfig)
	}
	switch block.Type {
	case "PUBLIC KEY":
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid PKIX public key", ErrConfig)
		}
		return k, nil
	case "RSA PUBLIC KEY":
		k, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid PKCS#1 public key", ErrConfig)
		}
		return k, nil
	case "CERTIFICATE":
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid certificate", ErrConfig)
		}
		switch cert.PublicKey.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey, ed25519.PublicKey:
			return cert.PublicKey, nil
		}
		return nil, errors.New("jwt: unsupported certificate key type")
	}
	return nil, fmt.Errorf("%w: unsupported PEM block %q", ErrConfig, block.Type)
}
