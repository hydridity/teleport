/*
Copyright 2015-2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/constants"
	"github.com/gravitational/teleport/api/identityfile"
	apiutils "github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/api/utils/sshutils"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// KeyIndex helps to identify a key in the store.
type KeyIndex struct {
	// ProxyHost is the root proxy hostname that a key is associated with.
	ProxyHost string
	// Username is the username that a key is associated with.
	Username string
	// ClusterName is the cluster name that a key is associated with.
	ClusterName string
}

// Check verifies the KeyIndex is fully specified.
func (idx KeyIndex) Check() error {
	missingField := "key index field %s is not set"
	if idx.ProxyHost == "" {
		return trace.BadParameter(missingField, "ProxyHost")
	}
	if idx.Username == "" {
		return trace.BadParameter(missingField, "Username")
	}
	if idx.ClusterName == "" {
		return trace.BadParameter(missingField, "ClusterName")
	}
	return nil
}

// ClientKey is a signed client key with certificates.
type ClientKey struct {
	KeyIndex

	KeyPair KeyPair

	// Cert is an SSH client certificate
	Cert []byte `json:"Cert,omitempty"`
	// TLSCert is a PEM encoded client TLS x509 certificate.
	// It's used to authenticate to the Teleport APIs.
	TLSCert []byte `json:"TLSCert,omitempty"`
	// KubeTLSCerts are TLS certificates (PEM-encoded) for individual
	// kubernetes clusters. Map key is a kubernetes cluster name.
	KubeTLSCerts map[string][]byte `json:"KubeCerts,omitempty"`
	// DBTLSCerts are PEM-encoded TLS certificates for database access.
	// Map key is the database service name.
	DBTLSCerts map[string][]byte `json:"DBCerts,omitempty"`
	// AppTLSCerts are TLS certificates for application access.
	// Map key is the application name.
	AppTLSCerts map[string][]byte `json:"AppCerts,omitempty"`
	// WindowsDesktopCerts are TLS certificates for Windows Desktop access.
	// Map key is the desktop server name.
	WindowsDesktopCerts map[string][]byte `json:"WindowsDesktopCerts,omitempty"`
	// TrustedCA is a list of trusted certificate authorities
	TrustedCA []auth.TrustedCerts
}

// NewClientKey returns a new unsigned client key, to be signed by
// a Teleport CA (Auth Server) for client certificates.
func NewClientKey() (*ClientKey, error) {
	clientKey := &ClientKey{
		KubeTLSCerts: make(map[string][]byte),
		DBTLSCerts:   make(map[string][]byte),
	}

	var err error
	clientKey.KeyPair, err = NewYkKeyPair()
	if err == nil {
		return clientKey, nil
	} else {
		return nil, trace.Wrap(err)
	}
	// } else if err != nil && !trace.IsNotFound(err) {
	// 	return nil, trace.Wrap(err)
	// }

	clientKey.KeyPair, err = NewPlainKeyPair()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return clientKey, nil
}

// TeleportClientTLSConfig returns client TLS configuration used
// to authenticate against API servers.
func (k *ClientKey) TeleportClientTLSConfig(cipherSuites []uint16, clusters []string) (*tls.Config, error) {
	return k.clientTLSConfig(cipherSuites, k.TLSCert, clusters)
}

func (k *ClientKey) clientTLSConfig(cipherSuites []uint16, tlsCertRaw []byte, clusters []string) (*tls.Config, error) {
	tlsCert, err := k.KeyPair.TLSCertificate(tlsCertRaw)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	pool := x509.NewCertPool()
	for _, caPEM := range k.TLSCAs() {
		cert, err := tlsca.ParseCertificatePEM(caPEM)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		for _, k := range clusters {
			if cert.Subject.CommonName == k {
				if !pool.AppendCertsFromPEM(caPEM) {
					return nil, trace.BadParameter("failed to parse TLS CA certificate")
				}
			}
		}
	}

	tlsConfig := utils.TLSConfig(cipherSuites)
	tlsConfig.RootCAs = pool
	tlsConfig.Certificates = append(tlsConfig.Certificates, tlsCert)

	// Use Issuer CN from the certificate to populate the correct SNI in
	// requests.
	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		return nil, trace.Wrap(err, "failed to parse TLS cert")
	}
	tlsConfig.ServerName = apiutils.EncodeClusterName(leaf.Issuer.CommonName)
	return tlsConfig, nil
}

// ProxyClientSSHConfig returns an ssh.ClientConfig with SSH credentials from this
// Key and HostKeyCallback matching SSH CAs in the Key.
//
// The config is set up to authenticate to proxy with the first available principal
// and ( if keyStore != nil ) trust local SSH CAs without asking for public keys.
//
func (k *ClientKey) ProxyClientSSHConfig(keyStore sshKnowHostGetter, host string) (*ssh.ClientConfig, error) {
	signer, err := k.KeyPair.SSHSigner()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	sshConfig, err := sshutils.ProxyClientSSHConfig(k.Cert, signer, k.SSHCAs())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if keyStore != nil {
		sshConfig.HostKeyCallback = NewKeyStoreCertChecker(keyStore, host)
	}

	return sshConfig, nil
}

// extractIdentityFromCert parses a tlsca.Identity from raw PEM cert bytes.
func extractIdentityFromCert(certBytes []byte) (*tlsca.Identity, error) {
	cert, err := tlsca.ParseCertificatePEM(certBytes)
	if err != nil {
		return nil, trace.Wrap(err, "failed to parse TLS certificate")
	}

	parsed, err := tlsca.FromSubject(cert.Subject, cert.NotAfter)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return parsed, nil
}

// KeyFromIdentityFile loads the private key + certificate
// from an identity file into a Key.
func KeyFromIdentityFile(path string) (*ClientKey, error) {
	ident, err := identityfile.ReadFile(path)
	if err != nil {
		return nil, trace.Wrap(err, "failed to parse identity file")
	}

	// validate both by parsing them:
	privKey, err := ssh.ParseRawPrivateKey(ident.PrivateKey)
	if err != nil {
		return nil, trace.BadParameter("invalid identity: %s. %v", path, err)
	}
	signer, err := ssh.NewSignerFromKey(privKey)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	dbTLSCerts := make(map[string][]byte)

	// validate TLS Cert (if present):
	if len(ident.Certs.TLS) > 0 {
		if _, err := tls.X509KeyPair(ident.Certs.TLS, ident.PrivateKey); err != nil {
			return nil, trace.Wrap(err)
		}

		parsedIdent, err := extractIdentityFromCert(ident.Certs.TLS)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		// If this identity file has any database certs, copy it into the DBTLSCerts map.
		if parsedIdent.RouteToDatabase.ServiceName != "" {
			dbTLSCerts[parsedIdent.RouteToDatabase.ServiceName] = ident.Certs.TLS
		}

		// TODO: add k8s, app, etc certs as well.
	}

	// Validate TLS CA certs (if present).
	var trustedCA []auth.TrustedCerts
	if len(ident.CACerts.TLS) > 0 || len(ident.CACerts.SSH) > 0 {
		trustedCA = []auth.TrustedCerts{{
			TLSCertificates:  ident.CACerts.TLS,
			HostCertificates: ident.CACerts.SSH,
		}}

		pool := x509.NewCertPool()
		for i, certPEM := range ident.CACerts.TLS {
			if !pool.AppendCertsFromPEM(certPEM) {
				return nil, trace.BadParameter("identity file contains invalid TLS CA cert (#%v)", i+1)
			}
		}

		for _, caCert := range ident.CACerts.SSH {
			if _, _, _, _, _, err := ssh.ParseKnownHosts(caCert); err != nil {
				return nil, trace.BadParameter("CA cert parsing error: %v; make sure this identity file was generated by 'tsh login -o' or 'tctl auth sign --format=file' or try generating it again", err.Error())
			}
		}
	}

	// TODO fix identity file support
	return &ClientKey{
		KeyPair: &PlainKeyPair{
			privateKeyRaw: ident.PrivateKey,
			publicKeyRaw:  ssh.MarshalAuthorizedKey(signer.PublicKey()),
		},
		Cert:       ident.Certs.SSH,
		TLSCert:    ident.Certs.TLS,
		TrustedCA:  trustedCA,
		DBTLSCerts: dbTLSCerts,
	}, nil
}

// RootClusterCAs returns root cluster CAs.
func (k *ClientKey) RootClusterCAs() ([][]byte, error) {
	rootClusterName, err := k.RootClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	var out [][]byte
	for _, cas := range k.TrustedCA {
		for _, v := range cas.TLSCertificates {
			cert, err := tlsca.ParseCertificatePEM(v)
			if err != nil {
				return nil, trace.Wrap(err)
			}
			if cert.Subject.CommonName == rootClusterName {
				out = append(out, v)
			}
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	return nil, trace.NotFound("failed to find TLS CA for %q root cluster", rootClusterName)
}

// TLSCAs returns all TLS CA certificates from this key
func (k *ClientKey) TLSCAs() (result [][]byte) {
	for _, ca := range k.TrustedCA {
		result = append(result, ca.TLSCertificates...)
	}
	return result
}

func (k *ClientKey) KubeClientTLSConfig(cipherSuites []uint16, kubeClusterName string) (*tls.Config, error) {
	rootCluster, err := k.RootClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tlsCert, ok := k.KubeTLSCerts[kubeClusterName]
	if !ok {
		return nil, trace.NotFound("TLS certificate for kubernetes cluster %q not found", kubeClusterName)
	}

	tlsConfig, err := k.clientTLSConfig(cipherSuites, tlsCert, []string{rootCluster})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tlsConfig.ServerName = fmt.Sprintf("%s%s", constants.KubeSNIPrefix, constants.APIDomain)
	return tlsConfig, nil
}

// SSHCAs returns all SSH CA certificates from this key
func (k *ClientKey) SSHCAs() (result [][]byte) {
	for _, ca := range k.TrustedCA {
		result = append(result, ca.HostCertificates...)
	}
	return result
}

// SSHCAsForClusters returns SSH CA for particular clusters.
func (k *ClientKey) SSHCAsForClusters(clusters []string) (result [][]byte, err error) {
	for _, ca := range k.TrustedCA {
		for _, hc := range ca.HostCertificates {
			_, hosts, _, _, _, err := ssh.ParseKnownHosts(hc)
			if err != nil {
				return nil, trace.Wrap(err)
			}

			for _, h := range hosts {
				for _, c := range clusters {
					if h == c {
						result = append(result, hc)
					}
				}
			}
		}
	}
	return result, nil
}

// CertUsername returns the name of the Teleport user encoded in the SSH certificate.
func (k *ClientKey) CertUsername() (string, error) {
	cert, err := k.SSHCert()
	if err != nil {
		return "", trace.Wrap(err)
	}
	return cert.KeyId, nil
}

// CertPrincipals returns the principals listed on the SSH certificate.
func (k *ClientKey) CertPrincipals() ([]string, error) {
	cert, err := k.SSHCert()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return cert.ValidPrincipals, nil
}

func (k *ClientKey) CertRoles() ([]string, error) {
	cert, err := k.SSHCert()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Extract roles from certificate. Note, if the certificate is in old format,
	// this will be empty.
	var roles []string
	rawRoles, ok := cert.Extensions[teleport.CertExtensionTeleportRoles]
	if ok {
		roles, err = services.UnmarshalCertRoles(rawRoles)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	return roles, nil
}

// AsAgentKeys converts client.Key struct to a []*agent.AddedKey. All elements
// of the []*agent.AddedKey slice need to be loaded into the agent!
func (k *ClientKey) AsAgentKeys() ([]agent.AddedKey, error) {
	cert, err := k.SSHCert()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return k.KeyPair.AsAgentKeys(cert)
}

// TeleportTLSCertificate returns the parsed x509 certificate for
// authentication against Teleport APIs.
func (k *ClientKey) TeleportTLSCertificate() (*x509.Certificate, error) {
	return tlsca.ParseCertificatePEM(k.TLSCert)
}

// KubeTLSCertificate returns the parsed x509 certificate for
// authentication against a named kubernetes cluster.
func (k *ClientKey) KubeTLSCertificate(kubeClusterName string) (*x509.Certificate, error) {
	tlsCert, ok := k.KubeTLSCerts[kubeClusterName]
	if !ok {
		return nil, trace.NotFound("TLS certificate for kubernetes cluster %q not found", kubeClusterName)
	}
	return tlsca.ParseCertificatePEM(tlsCert)
}

// DBTLSCertificates returns all parsed x509 database access certificates.
func (k *ClientKey) DBTLSCertificates() (certs []x509.Certificate, err error) {
	for _, bytes := range k.DBTLSCerts {
		cert, err := tlsca.ParseCertificatePEM(bytes)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		certs = append(certs, *cert)
	}
	return certs, nil
}

// AppTLSCertificates returns all parsed x509 app access certificates.
func (k *ClientKey) AppTLSCertificates() (certs []x509.Certificate, err error) {
	for _, bytes := range k.AppTLSCerts {
		cert, err := tlsca.ParseCertificatePEM(bytes)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		certs = append(certs, *cert)
	}
	return certs, nil
}

// TeleportTLSCertValidBefore returns the time of the TLS cert expiration
func (k *ClientKey) TeleportTLSCertValidBefore() (t time.Time, err error) {
	cert, err := k.TeleportTLSCertificate()
	if err != nil {
		return t, trace.Wrap(err)
	}
	return cert.NotAfter, nil
}

// CertValidBefore returns the time of the cert expiration
func (k *ClientKey) CertValidBefore() (t time.Time, err error) {
	cert, err := k.SSHCert()
	if err != nil {
		return t, trace.Wrap(err)
	}
	return time.Unix(int64(cert.ValidBefore), 0), nil
}

// AsAuthMethod returns an "auth method" interface, a common abstraction
// used by Golang SSH library. This is how you actually use a Key to feed
// it into the SSH lib.
func (k *ClientKey) AsAuthMethod() (ssh.AuthMethod, error) {
	signer, err := k.KeyPair.SSHSigner()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return ssh.PublicKeys(signer), nil
}

// AsSigner returns an ssh.Signer using the SSH certificate in this key.
func (k *ClientKey) AsSigner() (ssh.Signer, error) {
	return k.KeyPair.SSHSigner()
}

// SSHCert returns parsed SSH certificate
func (k *ClientKey) SSHCert() (*ssh.Certificate, error) {
	if k.Cert == nil {
		return nil, trace.NotFound("SSH cert not available")
	}
	return sshutils.ParseCertificate(k.Cert)
}

// ActiveRequests gets the active requests associated with this key.
func (k *ClientKey) ActiveRequests() (services.RequestIDs, error) {
	var activeRequests services.RequestIDs
	sshCert, err := k.SSHCert()
	if err != nil {
		return activeRequests, trace.Wrap(err)
	}
	rawRequests, ok := sshCert.Extensions[teleport.CertExtensionTeleportActiveRequests]
	if ok {
		if err := activeRequests.Unmarshal([]byte(rawRequests)); err != nil {
			return activeRequests, trace.Wrap(err)
		}
	}
	return activeRequests, nil
}

// CheckCert makes sure the SSH certificate is valid.
func (k *ClientKey) CheckCert() error {
	cert, err := k.SSHCert()
	if err != nil {
		return trace.Wrap(err)
	}

	// Check that the certificate was for the current public key. If not, the
	// public/private key pair may have been rotated.
	pub, _, _, _, err := ssh.ParseAuthorizedKey(k.KeyPair.PublicKeyRaw())
	if err != nil {
		return trace.Wrap(err)
	}
	if !sshutils.KeysEqual(cert.Key, pub) {
		return trace.CompareFailed("public key in profile does not match the public key in SSH certificate")
	}

	// A valid principal is always passed in because the principals are not being
	// checked here, but rather the validity period, signature, and algorithms.
	certChecker := sshutils.CertChecker{
		FIPS: isFIPS(),
	}
	err = certChecker.CheckCert(cert.ValidPrincipals[0], cert)
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// HostKeyCallback returns an ssh.HostKeyCallback that validates host
// keys/certs against SSH CAs in the Key.
//
// If not CAs are present in the Key, the returned ssh.HostKeyCallback is nil.
// This causes golang.org/x/crypto/ssh to prompt the user to verify host key
// fingerprint (same as OpenSSH does for an unknown host).
func (k *ClientKey) HostKeyCallback(withHostKeyFallback bool) (ssh.HostKeyCallback, error) {
	return sshutils.HostKeyCallback(k.SSHCAs(), withHostKeyFallback)
}

// HostKeyCallbackForClusters returns an ssh.HostKeyCallback that validates host
// keys/certs against SSH clusters CAs.
//
// If not CAs are present in the Key, the returned ssh.HostKeyCallback is nil.
// This causes golang.org/x/crypto/ssh to prompt the user to verify host key
// fingerprint (same as OpenSSH does for an unknown host).
func (k *ClientKey) HostKeyCallbackForClusters(withHostKeyFallback bool, clusters []string) (ssh.HostKeyCallback, error) {
	sshCA, err := k.SSHCAsForClusters(clusters)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return sshutils.HostKeyCallback(sshCA, withHostKeyFallback)
}

// RootClusterName extracts the root cluster name from the issuer
// of the Teleport TLS certificate.
func (k *ClientKey) RootClusterName() (string, error) {
	cert, err := k.TeleportTLSCertificate()
	if err != nil {
		return "", trace.Wrap(err)
	}
	clusterName := cert.Issuer.CommonName
	if clusterName == "" {
		return "", trace.NotFound("failed to extract root cluster name from Teleport TLS cert")
	}
	return clusterName, nil
}
