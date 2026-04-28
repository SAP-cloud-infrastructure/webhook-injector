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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/certificates"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/metrics"
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
)

// WebhookReconciler reconciles webhook ConfigMap and manages certificates
type WebhookReconciler struct {
	Client                client.Client // Hosting cluster client
	TargetClient          client.Client // Target cluster client
	WebhookConfigName     string
	Namespace             string
	CertSecretName        string
	CertValidity          time.Duration
	CertRotationThreshold float64
	Scheme                *runtime.Scheme
}

// Reconcile handles ConfigMap changes and certificate rotation
func (r *WebhookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
	caCert := secret.Data["tls.crt"]
	webhooks.InjectCABundle(configs, caCert)

	// Apply webhook configurations to target cluster
	if err := r.applyWebhookConfigurations(ctx, configs); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply webhook configurations: %w", err)
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
	certPEM, ok := secret.Data["tls.crt"]
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
	certPEM, ok := secret.Data["tls.crt"]
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

	// Time when rotation should happen: NotBefore + (totalValidity * threshold)
	rotationTime := cert.NotBefore.Add(time.Duration(float64(totalValidity) * r.CertRotationThreshold))

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
			"tls.crt": certPEM,
			"tls.key": keyPEM,
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

// updateCertExpiryGauge sets the certificate expiry metric from the secret's TLS cert.
func (r *WebhookReconciler) updateCertExpiryGauge(secret *corev1.Secret) {
	certPEM, ok := secret.Data["tls.crt"]
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
	return "default"
}

// SetupWithManager sets up the controller with the Manager
func (r *WebhookReconciler) SetupWithManager(mgr ctrl.Manager) error {
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
