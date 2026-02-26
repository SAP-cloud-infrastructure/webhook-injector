package controllers

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

	// TODO: Inject CA bundle and apply to target cluster (Part 2)

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

	// TODO: Check expiry and SANs - for now just return false if secret exists
	return secret, false, nil
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		Complete(r)
}
