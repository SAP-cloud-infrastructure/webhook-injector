// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/certificates"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/metrics"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/webhookhost"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/webhooks"
)

// rotationReason identifies why a certificate was rotated.
type rotationReason string

const (
	// ForceRotationAnnotation is the annotation key used to trigger certificate rotation.
	// When set to "true" on the certificate Secret, it forces immediate rotation.
	ForceRotationAnnotation = "webhook-injector.sap.com/force-rotation"

	rotationReasonInitial     rotationReason = "initial"
	rotationReasonForced      rotationReason = "force_rotation"
	rotationReasonExpired     rotationReason = "expired"
	rotationReasonSANMismatch rotationReason = "san_mismatch"

	tlsCrtKey        = "tls.crt"
	tlsKeyKey        = "tls.key"
	defaultNamespace = "default"
)

// WebhookReconciler reconciles webhook ConfigMap and manages certificates
type WebhookReconciler struct {
	Client       client.Client // Hosting cluster client
	TargetClient client.Client // Target cluster client

	// ConfigMap mode: name of the ConfigMap containing webhook YAML.
	// If empty, the reconciler runs in patch mode using WebhookLabelKey /
	// WebhookLabelValue. Exactly one of the two modes must be set.
	WebhookConfigName string

	// Label selector used to discover target-cluster resources whose caBundle
	// should be patched. clientConfig.URL and clientConfig.Service are left
	// unchanged. CRDs without a Webhook conversion strategy are logged and skipped.
	//
	// In patch mode (WebhookConfigName empty), the label applies to
	// MutatingWebhookConfigurations, ValidatingWebhookConfigurations, and
	// CustomResourceDefinitions.
	//
	// In ConfigMap mode (WebhookConfigName set), the label applies to
	// CustomResourceDefinitions only — labeled admission webhooks are ignored
	// because ConfigMap mode owns admission-webhook creation.
	WebhookLabelKey   string
	WebhookLabelValue string

	// ExternalHost, when non-empty, rewrites labeled MWC/VWC/CRD-conversion
	// clientConfig.Service entries to clientConfig.URL =
	// "https://<ExternalHost><Service.Path>" (Service.Path nil/empty → "/").
	// The Service field is then cleared. Entries that already use URL-form
	// clientConfig are left untouched. Patch mode only; main.go ignores the
	// flag in ConfigMap mode.
	ExternalHost string

	// CertSANs is a list of DNS names and/or IP addresses to include as certificate
	// SANs. Each entry is auto-classified: parses as net.ParseIP -> IP SAN; otherwise
	// DNS SAN. Required in patch mode (sole SAN source). In ConfigMap mode, these are
	// merged with SANs derived from the ConfigMap's webhook services/URLs.
	CertSANs []string

	// AdmissionWebhookName is the name of the MutatingWebhookConfiguration the
	// reconciler creates in the target cluster to register webhook-injector's own
	// admission mutator. Patch mode only; empty disables bootstrap. The MWC's
	// clientConfig.URL points at https://<AdmissionExternalHost>/mutate-{mwc,vwc,crd}
	// and its objectSelector matches WebhookLabelKey/Value so it never fires
	// against itself (the MWC itself is unlabeled).
	AdmissionWebhookName string

	// AdmissionExternalHost is the host:port the target cluster uses to reach
	// this controller's admission webhook server. Composed by main.go from the
	// host portion of --external-host and --admission-external-port, so the
	// admission endpoint and the labeled-webhook callbacks share the same host
	// but use independent ports. Patch mode only; required when
	// AdmissionWebhookName is non-empty.
	AdmissionExternalHost string

	Namespace             string
	CertSecretName        string
	CertValidity          time.Duration
	CertRotationThreshold float64
	Scheme                *runtime.Scheme
}

// Reconcile handles ConfigMap changes and certificate rotation
func (r *WebhookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.WebhookConfigName == "" {
		return r.reconcilePatchMode(ctx)
	}

	logger := log.FromContext(ctx)

	// Fetch the ConfigMap
	configMap := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, req.NamespacedName, configMap)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling webhook configuration", "configmap", req.NamespacedName)

	// Parse webhook configurations
	webhooksYAML, ok := configMap.Data["webhooks.yaml"]
	if !ok {
		return ctrl.Result{}, errors.New("webhooks.yaml not found in ConfigMap")
	}

	configs, err := webhooks.ParseWebhookConfigurations([]byte(webhooksYAML))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to parse webhook configurations: %w", err)
	}

	// Extract service references
	services := webhooks.ExtractServiceReferences(configs)
	// Append any user-supplied SANs (auto-classified DNS vs IP downstream).
	for _, san := range r.CertSANs {
		services = append(services, certificates.ServiceReference{Hostname: san})
	}
	if len(services) == 0 {
		logger.Info("No service references found in webhook configurations")
		return ctrl.Result{}, nil
	}

	// Check if certificates need generation/rotation
	secret, reason, err := r.checkCertificates(ctx, services)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check certificates: %w", err)
	}

	if reason != "" {
		logger.Info("Generating certificates", "services", len(services), "reason", reason)
		if err := r.generateAndStoreCertificates(ctx, services); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to generate certificates: %w", err)
		}
		metrics.CertificateRotationsTotal.WithLabelValues(string(reason)).Inc()

		// Re-fetch the secret after generation
		secret = &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      r.CertSecretName,
			Namespace: r.getNamespace(r.Namespace),
		}
		if err := r.Client.Get(ctx, secretKey, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get generated secret: %w", err)
		}
	}

	// Update expiry gauge from current certificate
	r.updateCertExpiryGauge(secret)

	// Inject CA bundle into webhook configurations (use tls.crt as it's self-signed)
	caCert := secret.Data[tlsCrtKey]
	webhooks.InjectCABundle(configs, caCert)

	// Apply webhook configurations to target cluster
	if err := r.applyWebhookConfigurations(ctx, configs); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply webhook configurations: %w", err)
	}

	// Patch CRD conversion webhooks (no-op when --webhook-label is unset)
	if err := r.patchCRDs(ctx, caCert); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch CRDs: %w", err)
	}

	logger.Info("Reconciliation complete")

	// Calculate next rotation time from certificate expiry
	nextRotation := r.calculateNextRotation(secret)
	if nextRotation > 0 {
		logger.Info("Scheduling next rotation check", "after", nextRotation)
		return ctrl.Result{RequeueAfter: nextRotation}, nil
	}

	// Fallback to sync period if we can't calculate rotation time
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// checkCertificates checks if certificates exist and are valid.
// Returns the rotation reason (empty string means no rotation needed).
func (r *WebhookReconciler) checkCertificates(ctx context.Context, services []certificates.ServiceReference) (*corev1.Secret, rotationReason, error) {
	secretKey := types.NamespacedName{
		Name:      r.CertSecretName,
		Namespace: r.getNamespace(r.Namespace),
	}

	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, secretKey, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, rotationReasonInitial, nil
		}
		return nil, "", err
	}

	// Check for force-rotation annotation
	if secret.Annotations != nil {
		if forceRotation, exists := secret.Annotations[ForceRotationAnnotation]; exists && forceRotation == "true" {
			return secret, rotationReasonForced, nil
		}
	}

	// Check if certificate needs rotation based on time/SANs
	reason, err := r.needsCertificateRotation(secret, services)
	if err != nil {
		return nil, "", fmt.Errorf("failed to check certificate rotation: %w", err)
	}

	return secret, reason, nil
}

// needsCertificateRotation checks if certificate needs rotation based on expiry and SANs.
// Returns the rotation reason or empty string if no rotation needed.
func (r *WebhookReconciler) needsCertificateRotation(secret *corev1.Secret, services []certificates.ServiceReference) (rotationReason, error) {
	// Get server certificate from secret
	certPEM, ok := secret.Data[tlsCrtKey]
	if !ok {
		return rotationReasonExpired, errors.New("tls.crt not found in secret")
	}

	// Parse PEM block
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return rotationReasonExpired, errors.New("failed to decode certificate PEM")
	}

	// Parse certificate
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return rotationReasonExpired, fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Check expiry - rotate if past threshold
	now := time.Now()
	totalValidity := cert.NotAfter.Sub(cert.NotBefore)
	remainingValidity := cert.NotAfter.Sub(now)

	if remainingValidity <= 0 {
		return rotationReasonExpired, nil
	}

	// Calculate remaining percentage
	remainingPercent := float64(remainingValidity) / float64(totalValidity)

	// Rotate if below threshold
	if remainingPercent < r.CertRotationThreshold {
		return rotationReasonExpired, nil
	}

	// Check if SANs match current service references
	expectedDNS, expectedIPs := buildExpectedSANs(services)
	currentDNS := make(map[string]bool)
	for _, dnsName := range cert.DNSNames {
		currentDNS[dnsName] = true
	}
	currentIPs := make(map[string]bool)
	for _, ip := range cert.IPAddresses {
		currentIPs[ip.String()] = true
	}

	// Check if all expected DNS SANs are present
	for san := range expectedDNS {
		if !currentDNS[san] {
			return rotationReasonSANMismatch, nil
		}
	}
	// Check for extra DNS SANs
	for san := range currentDNS {
		if !expectedDNS[san] {
			return rotationReasonSANMismatch, nil
		}
	}

	// Check if all expected IP SANs are present
	for ip := range expectedIPs {
		if !currentIPs[ip] {
			return rotationReasonSANMismatch, nil
		}
	}
	// Check for extra IP SANs
	for ip := range currentIPs {
		if !expectedIPs[ip] {
			return rotationReasonSANMismatch, nil
		}
	}

	return "", nil
}

// buildExpectedSANs builds the expected sets of DNS SANs and IP SANs from service references
func buildExpectedSANs(services []certificates.ServiceReference) (dns, ips map[string]bool) {
	dns = make(map[string]bool)
	ips = make(map[string]bool)
	for _, svc := range services {
		if svc.Hostname != "" {
			if ip := net.ParseIP(svc.Hostname); ip != nil {
				ips[ip.String()] = true
			} else {
				dns[svc.Hostname] = true
			}
		} else if svc.Name != "" && svc.Namespace != "" {
			dns[fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace)] = true
			dns[fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)] = true
		}
	}
	return dns, ips
}

// calculateNextRotation calculates when the next certificate rotation check should occur
// based on the certificate's expiry time and the configured rotation threshold.
// Returns the duration until the next check, or 0 if the certificate info can't be parsed.
func (r *WebhookReconciler) calculateNextRotation(secret *corev1.Secret) time.Duration {
	// Get server certificate from secret
	certPEM, ok := secret.Data[tlsCrtKey]
	if !ok {
		return 0
	}

	// Parse PEM block
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return 0
	}

	// Parse certificate
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return 0
	}

	// Calculate when the certificate will reach the rotation threshold
	now := time.Now()
	totalValidity := cert.NotAfter.Sub(cert.NotBefore)

	// Time when rotation should happen: when remaining validity drops below threshold
	rotationTime := cert.NotBefore.Add(time.Duration(float64(totalValidity) * (1 - r.CertRotationThreshold)))

	// Calculate duration until rotation time
	untilRotation := rotationTime.Sub(now)

	// If rotation time has passed or is very soon, return a short duration
	if untilRotation <= 0 {
		return time.Second
	}

	// Add a small buffer (1 second) to ensure we don't miss the rotation window
	return untilRotation + time.Second
}

// generateAndStoreCertificates generates certificates and stores them in a Secret
func (r *WebhookReconciler) generateAndStoreCertificates(ctx context.Context, services []certificates.ServiceReference) error {
	validity := r.CertValidity

	// Generate self-signed certificate
	certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(services, validity)
	if err != nil {
		return fmt.Errorf("failed to generate self-signed certificate: %w", err)
	}

	// Create or update Secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.CertSecretName,
			Namespace: r.getNamespace(r.Namespace),
			// Remove force-rotation annotation if present
			Annotations: map[string]string{},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			tlsCrtKey: certPEM,
			tlsKeyKey: keyPEM,
		},
	}

	existing := &corev1.Secret{}
	err = r.Client.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.Client.Create(ctx, secret)
		}
		return err
	}

	existing.Type = secret.Type
	existing.Data = secret.Data
	existing.Annotations = secret.Annotations
	return r.Client.Update(ctx, existing)
}

// applyWebhookConfigurations applies webhook configs to target cluster
func (r *WebhookReconciler) applyWebhookConfigurations(ctx context.Context, configs *webhooks.WebhookConfigurations) error {
	logger := log.FromContext(ctx)

	// Apply mutating webhook configurations
	for _, mwc := range configs.Mutating {
		logger.Info("Applying MutatingWebhookConfiguration", "name", mwc.Name)

		existing := &admissionv1.MutatingWebhookConfiguration{}
		err := r.TargetClient.Get(ctx, types.NamespacedName{Name: mwc.Name}, existing)

		if err != nil {
			if apierrors.IsNotFound(err) {
				// Create new
				if err := r.TargetClient.Create(ctx, mwc); err != nil {
					return fmt.Errorf("failed to create MutatingWebhookConfiguration %s: %w", mwc.Name, err)
				}
				logger.Info("Created MutatingWebhookConfiguration", "name", mwc.Name)
			} else {
				return fmt.Errorf("failed to get MutatingWebhookConfiguration %s: %w", mwc.Name, err)
			}
		} else {
			// Update existing
			existing.Webhooks = mwc.Webhooks
			if err := r.TargetClient.Update(ctx, existing); err != nil {
				return fmt.Errorf("failed to update MutatingWebhookConfiguration %s: %w", mwc.Name, err)
			}
			logger.Info("Updated MutatingWebhookConfiguration", "name", mwc.Name)
		}
	}

	// Apply validating webhook configurations
	for _, vwc := range configs.Validating {
		logger.Info("Applying ValidatingWebhookConfiguration", "name", vwc.Name)

		existing := &admissionv1.ValidatingWebhookConfiguration{}
		err := r.TargetClient.Get(ctx, types.NamespacedName{Name: vwc.Name}, existing)

		if err != nil {
			if apierrors.IsNotFound(err) {
				// Create new
				if err := r.TargetClient.Create(ctx, vwc); err != nil {
					return fmt.Errorf("failed to create ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
				}
				logger.Info("Created ValidatingWebhookConfiguration", "name", vwc.Name)
			} else {
				return fmt.Errorf("failed to get ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
			}
		} else {
			// Update existing
			existing.Webhooks = vwc.Webhooks
			if err := r.TargetClient.Update(ctx, existing); err != nil {
				return fmt.Errorf("failed to update ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
			}
			logger.Info("Updated ValidatingWebhookConfiguration", "name", vwc.Name)
		}
	}

	return nil
}

// reconcilePatchMode runs the alternative reconcile flow for pre-existing webhook
// configurations in the target cluster. It generates/rotates certificates whose SANs
// match CertSANs, then for each named MutatingWebhookConfiguration / Validating-
// WebhookConfiguration injects the freshly generated CA bundle into every webhook
// entry's clientConfig.CABundle. Existing clientConfig.URL and clientConfig.Service
// are left untouched. Missing target-cluster configurations are logged and skipped
// (never created).
func (r *WebhookReconciler) reconcilePatchMode(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if len(r.CertSANs) == 0 {
		return ctrl.Result{}, errors.New("cert-sans is empty in patch mode")
	}
	services := make([]certificates.ServiceReference, 0, len(r.CertSANs))
	for _, san := range r.CertSANs {
		services = append(services, certificates.ServiceReference{Hostname: san})
	}

	logger.Info("Reconciling in patch mode",
		"certSANs", r.CertSANs,
		"webhookLabel", r.WebhookLabelKey+"="+r.WebhookLabelValue,
	)

	secret, reason, err := r.checkCertificates(ctx, services)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check certificates: %w", err)
	}

	if reason != "" {
		logger.Info("Generating certificates", "reason", reason, "sans", r.CertSANs)
		if err := r.generateAndStoreCertificates(ctx, services); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to generate certificates: %w", err)
		}
		metrics.CertificateRotationsTotal.WithLabelValues(string(reason)).Inc()

		secret = &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      r.CertSecretName,
			Namespace: r.getNamespace(r.Namespace),
		}
		if err := r.Client.Get(ctx, secretKey, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get generated secret: %w", err)
		}
	}

	r.updateCertExpiryGauge(secret)

	caCert := secret.Data[tlsCrtKey]

	if err := r.bootstrapAdmissionWebhook(ctx, caCert); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to bootstrap admission webhook: %w", err)
	}

	if err := r.patchExistingWebhookConfigurations(ctx, caCert); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch webhook configurations: %w", err)
	}

	if err := r.patchCRDs(ctx, caCert); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch CRDs: %w", err)
	}

	logger.Info("Patch-mode reconciliation complete")

	nextRotation := r.calculateNextRotation(secret)
	if nextRotation > 0 {
		return ctrl.Result{RequeueAfter: nextRotation}, nil
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// buildExternalURL is a thin wrapper over webhookhost.BuildExternalURL kept
// for in-package readability.
func buildExternalURL(host string, svcPath *string) string {
	return webhookhost.BuildExternalURL(host, svcPath)
}

// patchExistingWebhookConfigurations lists MutatingWebhookConfigurations and
// ValidatingWebhookConfigurations in the target cluster matching the configured
// label selector, and rewrites only clientConfig.CABundle on every entry. The
// existing clientConfig.URL and clientConfig.Service fields are left untouched,
// unless ExternalHost is set: in that case Service-form clientConfig is
// rewritten to URL-form (URL = https://<ExternalHost><Service.Path>) and
// Service is cleared. URL-form entries are left alone.
// No-op when the label selector is unset, or when running in ConfigMap mode
// (which owns admission-webhook creation; labeled admission configs are
// intentionally ignored there).
func (r *WebhookReconciler) patchExistingWebhookConfigurations(ctx context.Context, caCert []byte) error {
	if r.WebhookConfigName != "" {
		return nil
	}
	if r.WebhookLabelKey == "" || r.WebhookLabelValue == "" {
		return nil
	}
	logger := log.FromContext(ctx)
	selector := client.MatchingLabels{r.WebhookLabelKey: r.WebhookLabelValue}
	labelStr := r.WebhookLabelKey + "=" + r.WebhookLabelValue

	var mwcList admissionv1.MutatingWebhookConfigurationList
	if err := r.TargetClient.List(ctx, &mwcList, selector); err != nil {
		return fmt.Errorf("failed to list MutatingWebhookConfigurations: %w", err)
	}
	if len(mwcList.Items) == 0 {
		logger.Info("No MutatingWebhookConfigurations matched label", "label", labelStr)
	}
	for i := range mwcList.Items {
		mwc := &mwcList.Items[i]
		rewroteURL := ""
		for j := range mwc.Webhooks {
			cc := &mwc.Webhooks[j].ClientConfig
			cc.CABundle = caCert
			if r.ExternalHost != "" && cc.Service != nil {
				url := buildExternalURL(r.ExternalHost, cc.Service.Path)
				cc.URL = &url
				cc.Service = nil
				rewroteURL = url
			}
		}
		if err := r.TargetClient.Update(ctx, mwc); err != nil {
			return fmt.Errorf("failed to update MutatingWebhookConfiguration %s: %w", mwc.Name, err)
		}
		if rewroteURL != "" {
			logger.Info("Rewrote clientConfig.Service to URL via --external-host",
				"kind", "MutatingWebhookConfiguration", "name", mwc.Name, "url", rewroteURL)
		}
		logger.Info("Patched MutatingWebhookConfiguration", "name", mwc.Name)
	}

	var vwcList admissionv1.ValidatingWebhookConfigurationList
	if err := r.TargetClient.List(ctx, &vwcList, selector); err != nil {
		return fmt.Errorf("failed to list ValidatingWebhookConfigurations: %w", err)
	}
	if len(vwcList.Items) == 0 {
		logger.Info("No ValidatingWebhookConfigurations matched label", "label", labelStr)
	}
	for i := range vwcList.Items {
		vwc := &vwcList.Items[i]
		rewroteURL := ""
		for j := range vwc.Webhooks {
			cc := &vwc.Webhooks[j].ClientConfig
			cc.CABundle = caCert
			if r.ExternalHost != "" && cc.Service != nil {
				url := buildExternalURL(r.ExternalHost, cc.Service.Path)
				cc.URL = &url
				cc.Service = nil
				rewroteURL = url
			}
		}
		if err := r.TargetClient.Update(ctx, vwc); err != nil {
			return fmt.Errorf("failed to update ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
		}
		if rewroteURL != "" {
			logger.Info("Rewrote clientConfig.Service to URL via --external-host",
				"kind", "ValidatingWebhookConfiguration", "name", vwc.Name, "url", rewroteURL)
		}
		logger.Info("Patched ValidatingWebhookConfiguration", "name", vwc.Name)
	}

	return nil
}

// patchCRDs lists CustomResourceDefinitions in the target cluster matching the
// configured label selector and updates only conversion-webhook
// clientConfig.CABundle on each match. clientConfig.URL and clientConfig.Service
// are left untouched, unless ExternalHost is set in patch mode: in that case
// Service-form clientConfig is rewritten to URL-form (URL =
// https://<ExternalHost><Service.Path>) and Service is cleared. CRDs that lack
// a Webhook conversion strategy or have no clientConfig are logged and skipped.
// No-op when the label selector is unset (ConfigMap mode without label-driven
// CRD patching). ExternalHost rewrites are skipped in ConfigMap mode even when
// the flag was passed (main.go is responsible for not propagating it there;
// this is a defense-in-depth check).
func (r *WebhookReconciler) patchCRDs(ctx context.Context, caCert []byte) error {
	if r.WebhookLabelKey == "" || r.WebhookLabelValue == "" {
		return nil
	}
	logger := log.FromContext(ctx)
	selector := client.MatchingLabels{r.WebhookLabelKey: r.WebhookLabelValue}
	labelStr := r.WebhookLabelKey + "=" + r.WebhookLabelValue

	var crdList apiextensionsv1.CustomResourceDefinitionList
	if err := r.TargetClient.List(ctx, &crdList, selector); err != nil {
		return fmt.Errorf("failed to list CustomResourceDefinitions: %w", err)
	}
	if len(crdList.Items) == 0 {
		logger.Info("No CustomResourceDefinitions matched label", "label", labelStr)
	}
	for i := range crdList.Items {
		crd := &crdList.Items[i]
		conv := crd.Spec.Conversion
		if conv == nil || conv.Strategy != apiextensionsv1.WebhookConverter || conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
			logger.Info("CRD has no Webhook conversion strategy, skipping", "name", crd.Name)
			continue
		}
		cc := conv.Webhook.ClientConfig
		cc.CABundle = caCert
		rewroteURL := ""
		if r.WebhookConfigName == "" && r.ExternalHost != "" && cc.Service != nil {
			url := buildExternalURL(r.ExternalHost, cc.Service.Path)
			cc.URL = &url
			cc.Service = nil
			rewroteURL = url
		}
		if err := r.TargetClient.Update(ctx, crd); err != nil {
			return fmt.Errorf("failed to update CustomResourceDefinition %s: %w", crd.Name, err)
		}
		if rewroteURL != "" {
			logger.Info("Rewrote clientConfig.Service to URL via --external-host",
				"kind", "CustomResourceDefinition", "name", crd.Name, "url", rewroteURL)
		}
		logger.Info("Patched CustomResourceDefinition conversion webhook", "name", crd.Name)
	}

	return nil
}

// bootstrapAdmissionWebhook creates or updates the MutatingWebhookConfiguration
// in the target cluster that registers webhook-injector's own admission mutator.
// It runs only in patch mode and only when AdmissionWebhookName and
// AdmissionExternalHost are set. The MWC's clientConfig.URL points at
// https://<AdmissionExternalHost>/mutate-{mwc,vwc,crd} and its objectSelector
// matches WebhookLabelKey/Value, so the MWC itself (which is unlabeled) is
// naturally excluded from its own mutation.
//
// failurePolicy is Ignore so an outage of webhook-injector does not block
// target-cluster creates/updates of MWC/VWC/CRD — the periodic reconcile loop
// (patchExistingWebhookConfigurations / patchCRDs) is the safety net for drift.
func (r *WebhookReconciler) bootstrapAdmissionWebhook(ctx context.Context, caCert []byte) error {
	if r.WebhookConfigName != "" {
		return nil // ConfigMap mode owns its own admission webhook lifecycle.
	}
	if r.AdmissionWebhookName == "" || r.AdmissionExternalHost == "" {
		return nil
	}
	if r.WebhookLabelKey == "" || r.WebhookLabelValue == "" {
		return nil
	}
	logger := log.FromContext(ctx)

	desired := r.desiredAdmissionWebhook(caCert)

	existing := &admissionv1.MutatingWebhookConfiguration{}
	err := r.TargetClient.Get(ctx, types.NamespacedName{Name: r.AdmissionWebhookName}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.TargetClient.Create(ctx, desired); err != nil {
				return fmt.Errorf("failed to create admission MWC %s: %w", r.AdmissionWebhookName, err)
			}
			logger.Info("Created admission MutatingWebhookConfiguration", "name", r.AdmissionWebhookName)
			return nil
		}
		return fmt.Errorf("failed to get admission MWC %s: %w", r.AdmissionWebhookName, err)
	}

	existing.Webhooks = desired.Webhooks
	// Force-clear labels on every reconcile so an external relabel cannot
	// stick — the objectSelector requires the configured webhook label, and
	// we never want our own MWC to match its own selector.
	existing.Labels = map[string]string{}
	if err := r.TargetClient.Update(ctx, existing); err != nil {
		return fmt.Errorf("failed to update admission MWC %s: %w", r.AdmissionWebhookName, err)
	}
	logger.Info("Updated admission MutatingWebhookConfiguration", "name", r.AdmissionWebhookName)
	return nil
}

// desiredAdmissionWebhook builds the desired MutatingWebhookConfiguration that
// registers webhook-injector's admission mutator with the target cluster. It
// has three webhook entries — one per kind — pointing at /mutate-mwc,
// /mutate-vwc, /mutate-crd on the hosting cluster's external host.
//
// Self-bootstrap exclusion: the MWC's objectSelector requires the configured
// --webhook-label, but the MWC itself is stamped with empty Labels here so
// the selector cannot match it even if some external actor (operator, GitOps
// reconciler, human with kubectl) tries to relabel it. The handler also
// short-circuits any object whose name matches AdmissionWebhookName as
// belt-and-suspenders.
//
// timeoutSeconds is set to 5s — admission must be fast and our handler is
// purely in-memory. reinvocationPolicy is intentionally NOT set: our
// transforms are idempotent but a re-invocation buys nothing, and Kubernetes
// defaults to Never which is what we want.
func (r *WebhookReconciler) desiredAdmissionWebhook(caCert []byte) *admissionv1.MutatingWebhookConfiguration {
	failurePolicy := admissionv1.Ignore
	sideEffects := admissionv1.SideEffectClassNone
	scopeAll := admissionv1.AllScopes
	timeoutSeconds := int32(5)

	objectSelector := &metav1.LabelSelector{
		MatchLabels: map[string]string{r.WebhookLabelKey: r.WebhookLabelValue},
	}

	mkClientConfig := func(path string) admissionv1.WebhookClientConfig {
		url := "https://" + r.AdmissionExternalHost + path
		return admissionv1.WebhookClientConfig{URL: &url, CABundle: caCert}
	}
	mkRule := func(group, resource string) admissionv1.RuleWithOperations {
		return admissionv1.RuleWithOperations{
			Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
			Rule: admissionv1.Rule{
				APIGroups:   []string{group},
				APIVersions: []string{"v1"},
				Resources:   []string{resource},
				Scope:       &scopeAll,
			},
		}
	}

	return &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.AdmissionWebhookName,
			// Pin to empty so the objectSelector below cannot match this MWC
			// itself, even if someone manually stamps the configured label.
			Labels: map[string]string{},
		},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				Name:                    "mwc.webhook-injector.sap.com",
				ClientConfig:            mkClientConfig("/mutate-mwc"),
				Rules:                   []admissionv1.RuleWithOperations{mkRule("admissionregistration.k8s.io", "mutatingwebhookconfigurations")},
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffects,
				TimeoutSeconds:          &timeoutSeconds,
				AdmissionReviewVersions: []string{"v1"},
				ObjectSelector:          objectSelector,
			},
			{
				Name:                    "vwc.webhook-injector.sap.com",
				ClientConfig:            mkClientConfig("/mutate-vwc"),
				Rules:                   []admissionv1.RuleWithOperations{mkRule("admissionregistration.k8s.io", "validatingwebhookconfigurations")},
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffects,
				TimeoutSeconds:          &timeoutSeconds,
				AdmissionReviewVersions: []string{"v1"},
				ObjectSelector:          objectSelector,
			},
			{
				Name:                    "crd.webhook-injector.sap.com",
				ClientConfig:            mkClientConfig("/mutate-crd"),
				Rules:                   []admissionv1.RuleWithOperations{mkRule("apiextensions.k8s.io", "customresourcedefinitions")},
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffects,
				TimeoutSeconds:          &timeoutSeconds,
				AdmissionReviewVersions: []string{"v1"},
				ObjectSelector:          objectSelector,
			},
		},
	}
}

// updateCertExpiryGauge sets the certificate expiry metric from the secret's TLS cert.
func (r *WebhookReconciler) updateCertExpiryGauge(secret *corev1.Secret) {
	certPEM, ok := secret.Data[tlsCrtKey]
	if !ok {
		return
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return
	}
	metrics.CertificateExpiryTime.Set(float64(cert.NotAfter.Unix()))
}

// getNamespace returns the namespace, using pod namespace as fallback
func (r *WebhookReconciler) getNamespace(namespace string) string {
	if namespace != "" {
		return namespace
	}
	// Try to read from pod
	if ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return string(ns)
	}
	return defaultNamespace
}

// SetupWithManager sets up the controller with the Manager
func (r *WebhookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.WebhookConfigName == "" {
		return r.setupPatchMode(mgr)
	}

	// Only watch the specific webhook ConfigMap
	configMapPredicate := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return r.WebhookConfigName == object.GetName() && r.getNamespace(r.Namespace) == object.GetNamespace()
	})

	// Map Secret events to ConfigMap reconcile requests
	// When the Secret changes (e.g., rotation annotation added), we reconcile the ConfigMap
	// IMPORTANT: Filter inside the mapper since WithEventFilter doesn't apply to secondary watches
	secretToConfigMapMapper := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		// Only map events for the specific certificate Secret
		if obj.GetName() != r.CertSecretName ||
			obj.GetNamespace() != r.getNamespace(r.Namespace) {
			return nil
		}

		return []ctrl.Request{
			{
				NamespacedName: types.NamespacedName{
					Name:      r.WebhookConfigName,
					Namespace: r.getNamespace(r.Namespace),
				},
			},
		}
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(configMapPredicate)).
		// Watch the certificate Secret for annotation-based rotation triggers
		Watches(&corev1.Secret{}, secretToConfigMapMapper).
		Complete(r)
}

// setupPatchMode wires the controller for patch mode. There is no primary
// ConfigMap and the certificate Secret may not yet exist (fresh install) — so
// anchoring on For(&Secret{}) alone would never enqueue a Reconcile until
// something external touched the Secret. To bootstrap, we:
//   - watch the certificate Secret (filtered to CertSecretName) for
//     subsequent rotation/annotation events,
//   - register a synthetic startup source that fires exactly one event when
//     the manager elects this replica leader, kicking off the first
//     Reconcile that creates the Secret.
//
// After the first Reconcile, the loop is self-driven: Reconcile returns
// ctrl.Result{RequeueAfter: ...} based on certificate expiry. The Secret
// predicate then catches subsequent annotation-driven rotations. Periodic
// drift in target-cluster webhook configurations is covered by RequeueAfter
// (and ultimately the manager's --sync-period for cached objects).
//
// The reconcile request key is ignored in patch mode — Reconcile branches on
// r.WebhookConfigName being empty.
func (r *WebhookReconciler) setupPatchMode(mgr ctrl.Manager) error {
	ns := r.getNamespace(r.Namespace)

	secretPredicate := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetName() == r.CertSecretName && object.GetNamespace() == ns
	})

	// Buffered so the bootstrap Runnable's send never blocks even if the
	// controller workers haven't started reading yet.
	bootstrapCh := make(chan event.GenericEvent, 1)
	bootstrap := &patchModeBootstrap{ch: bootstrapCh, namespace: ns, secretName: r.CertSecretName}
	if err := mgr.Add(bootstrap); err != nil {
		return fmt.Errorf("failed to register patch-mode bootstrap runnable: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("webhook-patch-mode").
		For(&corev1.Secret{}, builder.WithPredicates(secretPredicate)).
		WatchesRawSource(source.Channel(
			bootstrapCh,
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, _ client.Object) []ctrl.Request {
				return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: r.CertSecretName, Namespace: ns}}}
			}),
		)).
		Complete(r)
}

// patchModeBootstrap is a manager.Runnable that fires exactly one synthetic
// event into the patch-mode controller's channel source when Start is called.
// It runs on the leader (NeedLeaderElection returns true) so the bootstrap
// reconcile and the controller it wakes up are governed by the same lease.
type patchModeBootstrap struct {
	ch         chan event.GenericEvent
	namespace  string
	secretName string
}

// Start sends one GenericEvent and returns. The event payload is a placeholder
// ObjectMeta naming the cert Secret — the controller's mapper ignores its
// fields and always enqueues the same Request, but a non-nil Object is
// required by the Channel source.
func (b *patchModeBootstrap) Start(ctx context.Context) error {
	evt := event.GenericEvent{Object: &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: b.secretName, Namespace: b.namespace},
	}}
	select {
	case b.ch <- evt:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// NeedLeaderElection ensures the bootstrap only fires on the leader, matching
// the controller's own (default) leader-election behavior. Without this, all
// replicas would push a startup event and the followers' events would queue
// against a controller that isn't running on them.
func (b *patchModeBootstrap) NeedLeaderElection() bool { return true }

// Compile-time assertions that patchModeBootstrap satisfies the manager
// interfaces it needs.
var (
	_ manager.Runnable               = (*patchModeBootstrap)(nil)
	_ manager.LeaderElectionRunnable = (*patchModeBootstrap)(nil)
)
