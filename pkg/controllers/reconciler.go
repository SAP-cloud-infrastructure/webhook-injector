package controllers

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/certificates"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/webhooks"
)

// LeaderReconciler reconciles webhook ConfigMap and manages certificates
type LeaderReconciler struct {
	Client       client.Client // Hosting cluster client
	TargetClient client.Client // Target cluster client
	Config       *config.Config
	Scheme       *runtime.Scheme
}

// Reconcile handles ConfigMap changes and certificate rotation
func (r *LeaderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the ConfigMap
	configMap := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, req.NamespacedName, configMap)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reconciling webhook configuration", "configmap", req.NamespacedName)

	// Parse webhook configurations
	webhooksYAML, ok := configMap.Data["webhooks.yaml"]
	if !ok {
		return ctrl.Result{}, fmt.Errorf("webhooks.yaml not found in ConfigMap")
	}

	configs, err := webhooks.ParseWebhookConfigurations([]byte(webhooksYAML))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to parse webhook configurations: %w", err)
	}

	// Extract service references
	services := webhooks.ExtractServiceReferences(configs)
	if len(services) == 0 {
		log.Info("No service references found in webhook configurations")
		return ctrl.Result{}, nil
	}

	// Check if certificates need generation/rotation
	secret, needsGeneration, err := r.checkCertificates(ctx, services)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check certificates: %w", err)
	}

	if needsGeneration {
		log.Info("Generating certificates", "services", len(services))
		if err := r.generateAndStoreCertificates(ctx, services); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to generate certificates: %w", err)
		}

		// Re-fetch the secret after generation
		secret = &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      r.Config.CertSecretName,
			Namespace: r.getNamespace(r.Config.CertSecretNamespace),
		}
		if err := r.Client.Get(ctx, secretKey, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get generated secret: %w", err)
		}
	}

	// Inject CA bundle into webhook configurations
	caCert := secret.Data["ca.crt"]
	webhooks.InjectCABundle(configs, caCert)

	// Apply webhook configurations to target cluster
	if err := r.applyWebhookConfigurations(ctx, configs); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply webhook configurations: %w", err)
	}

	log.Info("Reconciliation complete")

	// Requeue after sync period
	return ctrl.Result{RequeueAfter: r.Config.SyncPeriod}, nil
}

// checkCertificates checks if certificates exist and are valid
func (r *LeaderReconciler) checkCertificates(ctx context.Context, services []certificates.ServiceReference) (*corev1.Secret, bool, error) {
	secretKey := types.NamespacedName{
		Name:      r.Config.CertSecretName,
		Namespace: r.getNamespace(r.Config.CertSecretNamespace),
	}

	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, secretKey, secret)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, true, nil // Needs generation
		}
		return nil, false, err
	}

	// Check if certificate needs rotation
	needsRotation, err := r.needsCertificateRotation(secret, services)
	if err != nil {
		return nil, false, fmt.Errorf("failed to check certificate rotation: %w", err)
	}

	return secret, needsRotation, nil
}

// needsCertificateRotation checks if certificate needs rotation based on expiry and SANs
func (r *LeaderReconciler) needsCertificateRotation(secret *corev1.Secret, services []certificates.ServiceReference) (bool, error) {
	// Get server certificate from secret
	certPEM, ok := secret.Data["tls.crt"]
	if !ok {
		return true, fmt.Errorf("tls.crt not found in secret")
	}

	// Parse PEM block
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return true, fmt.Errorf("failed to decode certificate PEM")
	}

	// Parse certificate
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return true, fmt.Errorf("failed to parse certificate: %w", err)
	}

	// Check expiry - rotate if past threshold
	now := time.Now()
	totalValidity := cert.NotAfter.Sub(cert.NotBefore)
	remainingValidity := cert.NotAfter.Sub(now)

	if remainingValidity <= 0 {
		// Certificate expired
		return true, nil
	}

	// Calculate remaining percentage
	remainingPercent := float64(remainingValidity) / float64(totalValidity)

	// Rotate if below threshold
	if remainingPercent < r.Config.CertRotationThreshold {
		return true, nil
	}

	// Check if SANs match current service references
	expectedSANs := buildExpectedSANs(services)
	currentSANs := make(map[string]bool)
	for _, dnsName := range cert.DNSNames {
		currentSANs[dnsName] = true
	}

	// Check if all expected SANs are present
	for san := range expectedSANs {
		if !currentSANs[san] {
			// Missing expected SAN, needs rotation
			return true, nil
		}
	}

	// Check if there are extra SANs (not strictly necessary, but good for cleanup)
	for san := range currentSANs {
		if !expectedSANs[san] {
			// Extra SAN present, needs rotation
			return true, nil
		}
	}

	return false, nil
}

// buildExpectedSANs builds the expected set of SANs from service references
func buildExpectedSANs(services []certificates.ServiceReference) map[string]bool {
	sans := make(map[string]bool)
	for _, svc := range services {
		if svc.Hostname != "" {
			// URL-based webhook - use hostname directly
			sans[svc.Hostname] = true
		} else if svc.Name != "" && svc.Namespace != "" {
			// Service-based webhook - add both short and FQDN forms
			sans[fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace)] = true
			sans[fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)] = true
		}
	}
	return sans
}

// generateAndStoreCertificates generates certificates and stores them in a Secret
func (r *LeaderReconciler) generateAndStoreCertificates(ctx context.Context, services []certificates.ServiceReference) error {
	validity := time.Duration(r.Config.CertValidityDays) * 24 * time.Hour

	// Generate CA
	caCert, caKey, err := certificates.GenerateCA(validity)
	if err != nil {
		return fmt.Errorf("failed to generate CA: %w", err)
	}

	// Generate server certificate
	serverCert, serverKey, err := certificates.GenerateServerCert(caCert, caKey, services, validity)
	if err != nil {
		return fmt.Errorf("failed to generate server certificate: %w", err)
	}

	// Create or update Secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.Config.CertSecretName,
			Namespace: r.getNamespace(r.Config.CertSecretNamespace),
		},
		Data: map[string][]byte{
			"ca.crt":  caCert,
			"ca.key":  caKey,
			"tls.crt": serverCert,
			"tls.key": serverKey,
		},
	}

	existing := &corev1.Secret{}
	err = r.Client.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, existing)
	if err != nil {
		if errors.IsNotFound(err) {
			return r.Client.Create(ctx, secret)
		}
		return err
	}

	existing.Data = secret.Data
	return r.Client.Update(ctx, existing)
}

// applyWebhookConfigurations applies webhook configs to target cluster
func (r *LeaderReconciler) applyWebhookConfigurations(ctx context.Context, configs *webhooks.WebhookConfigurations) error {
	log := log.FromContext(ctx)

	// Apply mutating webhook configurations
	for _, mwc := range configs.Mutating {
		log.Info("Applying MutatingWebhookConfiguration", "name", mwc.Name)

		existing := &admissionv1.MutatingWebhookConfiguration{}
		err := r.TargetClient.Get(ctx, types.NamespacedName{Name: mwc.Name}, existing)

		if err != nil {
			if errors.IsNotFound(err) {
				// Create new
				if err := r.TargetClient.Create(ctx, mwc); err != nil {
					return fmt.Errorf("failed to create MutatingWebhookConfiguration %s: %w", mwc.Name, err)
				}
				log.Info("Created MutatingWebhookConfiguration", "name", mwc.Name)
			} else {
				return fmt.Errorf("failed to get MutatingWebhookConfiguration %s: %w", mwc.Name, err)
			}
		} else {
			// Update existing
			existing.Webhooks = mwc.Webhooks
			if err := r.TargetClient.Update(ctx, existing); err != nil {
				return fmt.Errorf("failed to update MutatingWebhookConfiguration %s: %w", mwc.Name, err)
			}
			log.Info("Updated MutatingWebhookConfiguration", "name", mwc.Name)
		}
	}

	// Apply validating webhook configurations
	for _, vwc := range configs.Validating {
		log.Info("Applying ValidatingWebhookConfiguration", "name", vwc.Name)

		existing := &admissionv1.ValidatingWebhookConfiguration{}
		err := r.TargetClient.Get(ctx, types.NamespacedName{Name: vwc.Name}, existing)

		if err != nil {
			if errors.IsNotFound(err) {
				// Create new
				if err := r.TargetClient.Create(ctx, vwc); err != nil {
					return fmt.Errorf("failed to create ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
				}
				log.Info("Created ValidatingWebhookConfiguration", "name", vwc.Name)
			} else {
				return fmt.Errorf("failed to get ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
			}
		} else {
			// Update existing
			existing.Webhooks = vwc.Webhooks
			if err := r.TargetClient.Update(ctx, existing); err != nil {
				return fmt.Errorf("failed to update ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
			}
			log.Info("Updated ValidatingWebhookConfiguration", "name", vwc.Name)
		}
	}

	return nil
}

// getNamespace returns the namespace, using pod namespace as fallback
func (r *LeaderReconciler) getNamespace(namespace string) string {
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
func (r *LeaderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only watch the specific webhook ConfigMap
	configMapPredicate := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetName() == r.Config.WebhookConfigName &&
			object.GetNamespace() == r.getNamespace(r.Config.WebhookConfigNamespace)
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(configMapPredicate).
		Complete(r)
}
