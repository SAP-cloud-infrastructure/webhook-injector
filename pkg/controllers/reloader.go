package controllers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
)

// CertificateReloader watches the certificate Secret and writes certs to disk
type CertificateReloader struct {
	Client client.Client
	Config *config.Config
}

// Reconcile reads certificates from Secret and writes to disk
func (r *CertificateReloader) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Fetch the Secret
	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, req.NamespacedName, secret)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("Reloading certificates from Secret", "secret", req.NamespacedName)

	// Write certificates to disk
	certFiles := map[string]string{
		"ca.crt":  "ca.crt",
		"tls.crt": "tls.crt",
		"tls.key": "tls.key",
	}

	for secretKey, fileName := range certFiles {
		data, ok := secret.Data[secretKey]
		if !ok {
			log.Error(fmt.Errorf("missing key in secret"), "Missing certificate data", "key", secretKey)
			continue
		}

		if err := r.writeCertFile(fileName, data); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to write %s: %w", fileName, err)
		}
	}

	log.Info("Certificates reloaded successfully")
	return ctrl.Result{}, nil
}

// writeCertFile writes certificate data to disk atomically (temp file + rename)
func (r *CertificateReloader) writeCertFile(fileName string, data []byte) error {
	targetPath := filepath.Join(r.Config.CertDir, fileName)
	tempPath := targetPath + ".tmp"

	// Write to temp file
	if err := os.WriteFile(tempPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, targetPath); err != nil {
		os.Remove(tempPath) // Clean up temp file on error
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *CertificateReloader) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}).
		Complete(r)
}
