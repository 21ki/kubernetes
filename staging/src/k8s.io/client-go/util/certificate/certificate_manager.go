/*
Copyright 2017 The Kubernetes Authors.

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

package certificate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"sync"
	"time"

	"k8s.io/klog/v2"

	certificates "k8s.io/api/certificates/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/certificate/csr"
	"k8s.io/client-go/util/keyutil"
)

// certificateWaitTimeout controls the amount of time we wait for certificate
// approval in one iteration.
var certificateWaitTimeout = 15 * time.Minute

// Manager maintains and updates the certificates in use by this certificate
// manager. In the background it communicates with the API server to get new
// certificates for certificates about to expire.
type Manager interface {
	// Start the API server status sync loop.
	Start()
	// Stop the cert manager loop.
	Stop()
	// Current returns the currently selected certificate from the
	// certificate manager, as well as the associated certificate and key data
	// in PEM format.
	Current() *tls.Certificate
	// ServerHealthy returns true if the manager is able to communicate with
	// the server. This allows a caller to determine whether the cert manager
	// thinks it can potentially talk to the API server. The cert manager may
	// be very conservative and only return true if recent communication has
	// occurred with the server.
	ServerHealthy() bool
}

// Config is the set of configuration parameters available for a new Manager.
type Config struct {
	// ClientsetFn will be used to create a clientset for
	// creating/fetching new certificate requests generated when a key rotation occurs.
	// The function will never be invoked in parallel.
	// It is passed the current client certificate if one exists.
	ClientsetFn ClientsetFunc
	// Template is the CertificateRequest that will be used as a template for
	// generating certificate signing requests for all new keys generated as
	// part of rotation. It follows the same rules as the template parameter of
	// crypto.x509.CreateCertificateRequest in the Go standard libraries.
	Template *x509.CertificateRequest
	// GetTemplate returns the CertificateRequest that will be used as a template for
	// generating certificate signing requests for all new keys generated as
	// part of rotation. It follows the same rules as the template parameter of
	// crypto.x509.CreateCertificateRequest in the Go standard libraries.
	// If no template is available, nil may be returned, and no certificate will be requested.
	// If specified, takes precedence over Template.
	GetTemplate func() *x509.CertificateRequest
	// SignerName is the name of the certificate signer that should sign certificates
	// generated by the manager.
	SignerName string
	// Usages is the types of usages that certificates generated by the manager
	// can be used for.
	Usages []certificates.KeyUsage
	// CertificateStore is a persistent store where the current cert/key is
	// kept and future cert/key pairs will be persisted after they are
	// generated.
	CertificateStore Store
	// BootstrapCertificatePEM is the certificate data that will be returned
	// from the Manager if the CertificateStore doesn't have any cert/key pairs
	// currently available and has not yet had a chance to get a new cert/key
	// pair from the API. If the CertificateStore does have a cert/key pair,
	// this will be ignored. If there is no cert/key pair available in the
	// CertificateStore, as soon as Start is called, it will request a new
	// cert/key pair from the CertificateSigningRequestClient. This is intended
	// to allow the first boot of a component to be initialized using a
	// generic, multi-use cert/key pair which will be quickly replaced with a
	// unique cert/key pair.
	BootstrapCertificatePEM []byte
	// BootstrapKeyPEM is the key data that will be returned from the Manager
	// if the CertificateStore doesn't have any cert/key pairs currently
	// available. If the CertificateStore does have a cert/key pair, this will
	// be ignored. If the bootstrap cert/key pair are used, they will be
	// rotated at the first opportunity, possibly well in advance of expiring.
	// This is intended to allow the first boot of a component to be
	// initialized using a generic, multi-use cert/key pair which will be
	// quickly replaced with a unique cert/key pair.
	BootstrapKeyPEM []byte `datapolicy:"security-key"`
	// CertificateRotation will record a metric showing the time in seconds
	// that certificates lived before being rotated. This metric is a histogram
	// because there is value in keeping a history of rotation cadences. It
	// allows one to setup monitoring and alerting of unexpected rotation
	// behavior and track trends in rotation frequency.
	CertificateRotation Histogram
	// CertifcateRenewFailure will record a metric that keeps track of
	// certificate renewal failures.
	CertificateRenewFailure Counter
}

// Store is responsible for getting and updating the current certificate.
// Depending on the concrete implementation, the backing store for this
// behavior may vary.
type Store interface {
	// Current returns the currently selected certificate, as well as the
	// associated certificate and key data in PEM format. If the Store doesn't
	// have a cert/key pair currently, it should return a NoCertKeyError so
	// that the Manager can recover by using bootstrap certificates to request
	// a new cert/key pair.
	Current() (*tls.Certificate, error)
	// Update accepts the PEM data for the cert/key pair and makes the new
	// cert/key pair the 'current' pair, that will be returned by future calls
	// to Current().
	Update(cert, key []byte) (*tls.Certificate, error)
}

// Gauge will record the remaining lifetime of the certificate each time it is
// updated.
type Gauge interface {
	Set(float64)
}

// Histogram will record the time a rotated certificate was used before being
// rotated.
type Histogram interface {
	Observe(float64)
}

// Counter will wrap a counter with labels
type Counter interface {
	Inc()
}

// NoCertKeyError indicates there is no cert/key currently available.
type NoCertKeyError string

// ClientsetFunc returns a new clientset for discovering CSR API availability and requesting CSRs.
// It is passed the current certificate if one is available and valid.
type ClientsetFunc func(current *tls.Certificate) (clientset.Interface, error)

func (e *NoCertKeyError) Error() string { return string(*e) }

type manager struct {
	getTemplate func() *x509.CertificateRequest

	// lastRequestLock guards lastRequestCancel and lastRequest
	lastRequestLock   sync.Mutex
	lastRequestCancel context.CancelFunc
	lastRequest       *x509.CertificateRequest

	dynamicTemplate bool
	signerName      string
	usages          []certificates.KeyUsage
	forceRotation   bool

	certStore Store

	certificateRotation     Histogram
	certificateRenewFailure Counter

	// the following variables must only be accessed under certAccessLock
	certAccessLock sync.RWMutex
	cert           *tls.Certificate
	serverHealth   bool

	// the clientFn must only be accessed under the clientAccessLock
	clientAccessLock sync.Mutex
	clientsetFn      ClientsetFunc
	stopCh           chan struct{}
	stopped          bool

	// Set to time.Now but can be stubbed out for testing
	now func() time.Time
}

// NewManager returns a new certificate manager. A certificate manager is
// responsible for being the authoritative source of certificates in the
// Kubelet and handling updates due to rotation.
func NewManager(config *Config) (Manager, error) {
	cert, forceRotation, err := getCurrentCertificateOrBootstrap(
		config.CertificateStore,
		config.BootstrapCertificatePEM,
		config.BootstrapKeyPEM)
	if err != nil {
		return nil, err
	}

	getTemplate := config.GetTemplate
	if getTemplate == nil {
		getTemplate = func() *x509.CertificateRequest { return config.Template }
	}

	m := manager{
		stopCh:                  make(chan struct{}),
		clientsetFn:             config.ClientsetFn,
		getTemplate:             getTemplate,
		dynamicTemplate:         config.GetTemplate != nil,
		signerName:              config.SignerName,
		usages:                  config.Usages,
		certStore:               config.CertificateStore,
		cert:                    cert,
		forceRotation:           forceRotation,
		certificateRotation:     config.CertificateRotation,
		certificateRenewFailure: config.CertificateRenewFailure,
		now:                     time.Now,
	}

	return &m, nil
}

// Current returns the currently selected certificate from the certificate
// manager. This can be nil if the manager was initialized without a
// certificate and has not yet received one from the
// CertificateSigningRequestClient, or if the current cert has expired.
func (m *manager) Current() *tls.Certificate {
	m.certAccessLock.RLock()
	defer m.certAccessLock.RUnlock()
	if m.cert != nil && m.cert.Leaf != nil && m.now().After(m.cert.Leaf.NotAfter) {
		klog.V(2).Infof("Current certificate is expired.")
		return nil
	}
	return m.cert
}

// ServerHealthy returns true if the cert manager believes the server
// is currently alive.
func (m *manager) ServerHealthy() bool {
	m.certAccessLock.RLock()
	defer m.certAccessLock.RUnlock()
	return m.serverHealth
}

// Stop terminates the manager.
func (m *manager) Stop() {
	m.clientAccessLock.Lock()
	defer m.clientAccessLock.Unlock()
	if m.stopped {
		return
	}
	close(m.stopCh)
	m.stopped = true
}

// Start will start the background work of rotating the certificates.
func (m *manager) Start() {
	// Certificate rotation depends on access to the API server certificate
	// signing API, so don't start the certificate manager if we don't have a
	// client.
	if m.clientsetFn == nil {
		klog.V(2).Infof("Certificate rotation is not enabled, no connection to the apiserver.")
		return
	}

	klog.V(2).Infof("Certificate rotation is enabled.")

	templateChanged := make(chan struct{})
	go wait.Until(func() {
		deadline := m.nextRotationDeadline()
		if sleepInterval := deadline.Sub(m.now()); sleepInterval > 0 {
			klog.V(2).Infof("Waiting %v for next certificate rotation", sleepInterval)

			timer := time.NewTimer(sleepInterval)
			defer timer.Stop()

			select {
			case <-timer.C:
				// unblock when deadline expires
			case <-templateChanged:
				_, lastRequestTemplate := m.getLastRequest()
				if reflect.DeepEqual(lastRequestTemplate, m.getTemplate()) {
					// if the template now matches what we last requested, restart the rotation deadline loop
					return
				}
				klog.V(2).Infof("Certificate template changed, rotating")
			}
		}

		// Don't enter rotateCerts and trigger backoff if we don't even have a template to request yet
		if m.getTemplate() == nil {
			return
		}

		backoff := wait.Backoff{
			Duration: 2 * time.Second,
			Factor:   2,
			Jitter:   0.1,
			Steps:    5,
		}
		if err := wait.ExponentialBackoff(backoff, m.rotateCerts); err != nil {
			utilruntime.HandleError(fmt.Errorf("Reached backoff limit, still unable to rotate certs: %v", err))
			wait.PollInfinite(32*time.Second, m.rotateCerts)
		}
	}, time.Second, m.stopCh)

	if m.dynamicTemplate {
		go wait.Until(func() {
			// check if the current template matches what we last requested
			lastRequestCancel, lastRequestTemplate := m.getLastRequest()

			if !m.certSatisfiesTemplate() && !reflect.DeepEqual(lastRequestTemplate, m.getTemplate()) {
				// if the template is different, queue up an interrupt of the rotation deadline loop.
				// if we've requested a CSR that matches the new template by the time the interrupt is handled, the interrupt is disregarded.
				if lastRequestCancel != nil {
					// if we're currently waiting on a submitted request that no longer matches what we want, stop waiting
					lastRequestCancel()
				}
				select {
				case templateChanged <- struct{}{}:
				case <-m.stopCh:
				}
			}
		}, time.Second, m.stopCh)
	}
}

func getCurrentCertificateOrBootstrap(
	store Store,
	bootstrapCertificatePEM []byte,
	bootstrapKeyPEM []byte) (cert *tls.Certificate, shouldRotate bool, errResult error) {

	currentCert, err := store.Current()
	if err == nil {
		// if the current cert is expired, fall back to the bootstrap cert
		if currentCert.Leaf != nil && time.Now().Before(currentCert.Leaf.NotAfter) {
			return currentCert, false, nil
		}
	} else {
		if _, ok := err.(*NoCertKeyError); !ok {
			return nil, false, err
		}
	}

	if bootstrapCertificatePEM == nil || bootstrapKeyPEM == nil {
		return nil, true, nil
	}

	bootstrapCert, err := tls.X509KeyPair(bootstrapCertificatePEM, bootstrapKeyPEM)
	if err != nil {
		return nil, false, err
	}
	if len(bootstrapCert.Certificate) < 1 {
		return nil, false, fmt.Errorf("no cert/key data found")
	}

	certs, err := x509.ParseCertificates(bootstrapCert.Certificate[0])
	if err != nil {
		return nil, false, fmt.Errorf("unable to parse certificate data: %v", err)
	}
	if len(certs) < 1 {
		return nil, false, fmt.Errorf("no cert data found")
	}
	bootstrapCert.Leaf = certs[0]

	if _, err := store.Update(bootstrapCertificatePEM, bootstrapKeyPEM); err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to set the cert/key pair to the bootstrap certificate: %v", err))
	} else {
		klog.V(4).Infof("Updated the store to contain the initial bootstrap certificate")
	}

	return &bootstrapCert, true, nil
}

func (m *manager) getClientset() (clientset.Interface, error) {
	current := m.Current()
	m.clientAccessLock.Lock()
	defer m.clientAccessLock.Unlock()
	return m.clientsetFn(current)
}

// RotateCerts is exposed for testing only and is not a part of the public interface.
// Returns true if it changed the cert, false otherwise. Error is only returned in
// exceptional cases.
func (m *manager) RotateCerts() (bool, error) {
	return m.rotateCerts()
}

// rotateCerts attempts to request a client cert from the server, wait a reasonable
// period of time for it to be signed, and then update the cert on disk. If it cannot
// retrieve a cert, it will return false. It will only return error in exceptional cases.
// This method also keeps track of "server health" by interpreting the responses it gets
// from the server on the various calls it makes.
// TODO: return errors, have callers handle and log them correctly
func (m *manager) rotateCerts() (bool, error) {
	klog.V(2).Infof("Rotating certificates")

	template, csrPEM, keyPEM, privateKey, err := m.generateCSR()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to generate a certificate signing request: %v", err))
		if m.certificateRenewFailure != nil {
			m.certificateRenewFailure.Inc()
		}
		return false, nil
	}

	// request the client each time
	clientSet, err := m.getClientset()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to load a client to request certificates: %v", err))
		if m.certificateRenewFailure != nil {
			m.certificateRenewFailure.Inc()
		}
		return false, nil
	}

	// Call the Certificate Signing Request API to get a certificate for the
	// new private key.
	reqName, reqUID, err := csr.RequestCertificate(clientSet, csrPEM, "", m.signerName, m.usages, privateKey)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Failed while requesting a signed certificate from the master: %v", err))
		if m.certificateRenewFailure != nil {
			m.certificateRenewFailure.Inc()
		}
		return false, m.updateServerError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), certificateWaitTimeout)
	defer cancel()

	// Once we've successfully submitted a CSR for this template, record that we did so
	m.setLastRequest(cancel, template)

	// Wait for the certificate to be signed. This interface and internal timout
	// is a remainder after the old design using raw watch wrapped with backoff.
	crtPEM, err := csr.WaitForCertificate(ctx, clientSet, reqName, reqUID)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("certificate request was not signed: %v", err))
		if m.certificateRenewFailure != nil {
			m.certificateRenewFailure.Inc()
		}
		return false, nil
	}

	cert, err := m.certStore.Update(crtPEM, keyPEM)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to store the new cert/key pair: %v", err))
		if m.certificateRenewFailure != nil {
			m.certificateRenewFailure.Inc()
		}
		return false, nil
	}

	if old := m.updateCached(cert); old != nil && m.certificateRotation != nil {
		m.certificateRotation.Observe(m.now().Sub(old.Leaf.NotBefore).Seconds())
	}

	return true, nil
}

// Check that the current certificate on disk satisfies the requests from the
// current template.
//
// Note that extra items in the certificate's SAN or orgs that don't exist in
// the template will not trigger a renewal.
//
// Requires certAccessLock to be locked.
func (m *manager) certSatisfiesTemplateLocked() bool {
	if m.cert == nil {
		return false
	}

	if template := m.getTemplate(); template != nil {
		if template.Subject.CommonName != m.cert.Leaf.Subject.CommonName {
			klog.V(2).Infof("Current certificate CN (%s) does not match requested CN (%s)", m.cert.Leaf.Subject.CommonName, template.Subject.CommonName)
			return false
		}

		currentDNSNames := sets.NewString(m.cert.Leaf.DNSNames...)
		desiredDNSNames := sets.NewString(template.DNSNames...)
		missingDNSNames := desiredDNSNames.Difference(currentDNSNames)
		if len(missingDNSNames) > 0 {
			klog.V(2).Infof("Current certificate is missing requested DNS names %v", missingDNSNames.List())
			return false
		}

		currentIPs := sets.NewString()
		for _, ip := range m.cert.Leaf.IPAddresses {
			currentIPs.Insert(ip.String())
		}
		desiredIPs := sets.NewString()
		for _, ip := range template.IPAddresses {
			desiredIPs.Insert(ip.String())
		}
		missingIPs := desiredIPs.Difference(currentIPs)
		if len(missingIPs) > 0 {
			klog.V(2).Infof("Current certificate is missing requested IP addresses %v", missingIPs.List())
			return false
		}

		currentOrgs := sets.NewString(m.cert.Leaf.Subject.Organization...)
		desiredOrgs := sets.NewString(template.Subject.Organization...)
		missingOrgs := desiredOrgs.Difference(currentOrgs)
		if len(missingOrgs) > 0 {
			klog.V(2).Infof("Current certificate is missing requested orgs %v", missingOrgs.List())
			return false
		}
	}

	return true
}

func (m *manager) certSatisfiesTemplate() bool {
	m.certAccessLock.RLock()
	defer m.certAccessLock.RUnlock()
	return m.certSatisfiesTemplateLocked()
}

// nextRotationDeadline returns a value for the threshold at which the
// current certificate should be rotated, 80%+/-10% of the expiration of the
// certificate.
func (m *manager) nextRotationDeadline() time.Time {
	// forceRotation is not protected by locks
	if m.forceRotation {
		m.forceRotation = false
		return m.now()
	}

	m.certAccessLock.RLock()
	defer m.certAccessLock.RUnlock()

	if !m.certSatisfiesTemplateLocked() {
		return m.now()
	}

	notAfter := m.cert.Leaf.NotAfter
	totalDuration := float64(notAfter.Sub(m.cert.Leaf.NotBefore))
	deadline := m.cert.Leaf.NotBefore.Add(jitteryDuration(totalDuration))

	klog.V(2).Infof("Certificate expiration is %v, rotation deadline is %v", notAfter, deadline)
	return deadline
}

// jitteryDuration uses some jitter to set the rotation threshold so each node
// will rotate at approximately 70-90% of the total lifetime of the
// certificate.  With jitter, if a number of nodes are added to a cluster at
// approximately the same time (such as cluster creation time), they won't all
// try to rotate certificates at the same time for the rest of the life of the
// cluster.
//
// This function is represented as a variable to allow replacement during testing.
var jitteryDuration = func(totalDuration float64) time.Duration {
	return wait.Jitter(time.Duration(totalDuration), 0.2) - time.Duration(totalDuration*0.3)
}

// updateCached sets the most recent retrieved cert and returns the old cert.
// It also sets the server as assumed healthy.
func (m *manager) updateCached(cert *tls.Certificate) *tls.Certificate {
	m.certAccessLock.Lock()
	defer m.certAccessLock.Unlock()
	m.serverHealth = true
	old := m.cert
	m.cert = cert
	return old
}

// updateServerError takes an error returned by the server and infers
// the health of the server based on the error. It will return nil if
// the error does not require immediate termination of any wait loops,
// and otherwise it will return the error.
func (m *manager) updateServerError(err error) error {
	m.certAccessLock.Lock()
	defer m.certAccessLock.Unlock()
	switch {
	case errors.IsUnauthorized(err):
		// SSL terminating proxies may report this error instead of the master
		m.serverHealth = true
	case errors.IsUnexpectedServerError(err):
		// generally indicates a proxy or other load balancer problem, rather than a problem coming
		// from the master
		m.serverHealth = false
	default:
		// Identify known errors that could be expected for a cert request that
		// indicate everything is working normally
		m.serverHealth = errors.IsNotFound(err) || errors.IsForbidden(err)
	}
	return nil
}

func (m *manager) generateCSR() (template *x509.CertificateRequest, csrPEM []byte, keyPEM []byte, key interface{}, err error) {
	// Generate a new private key.
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("unable to generate a new private key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("unable to marshal the new key to DER: %v", err)
	}

	keyPEM = pem.EncodeToMemory(&pem.Block{Type: keyutil.ECPrivateKeyBlockType, Bytes: der})

	template = m.getTemplate()
	if template == nil {
		return nil, nil, nil, nil, fmt.Errorf("unable to create a csr, no template available")
	}
	csrPEM, err = cert.MakeCSRFromTemplate(privateKey, template)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("unable to create a csr from the private key: %v", err)
	}
	return template, csrPEM, keyPEM, privateKey, nil
}

func (m *manager) getLastRequest() (context.CancelFunc, *x509.CertificateRequest) {
	m.lastRequestLock.Lock()
	defer m.lastRequestLock.Unlock()
	return m.lastRequestCancel, m.lastRequest
}

func (m *manager) setLastRequest(cancel context.CancelFunc, r *x509.CertificateRequest) {
	m.lastRequestLock.Lock()
	defer m.lastRequestLock.Unlock()
	m.lastRequestCancel = cancel
	m.lastRequest = r
}
