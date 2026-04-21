// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

var (
	webhookConfigName     string
	namespace             string
	certSecretName        string
	certDir               string
	certValidity          time.Duration
	certRotationThreshold float64
	targetKubeconfig      string
	enableLeaderElection  bool
	leaderElectionID      string
	syncPeriod            time.Duration
	metricsAddr           string
	healthAddr            string
)

func init() {
	// Initialize flag variables
	flag.StringVar(&webhookConfigName, "webhook-config-name", "", "ConfigMap name containing webhook definitions (required)")
	flag.StringVar(&namespace, "namespace", "", "Namespace for all resources (auto-detects from pod if not set)")
	flag.StringVar(&certSecretName, "cert-secret-name", "webhook-injector-certs", "Secret name for storing certificates")
	flag.StringVar(&certDir, "cert-dir", "/tmp/webhook-certs", "Directory to write certificate files")
	flag.DurationVar(&certValidity, "cert-validity", 720*time.Hour, "Certificate validity duration")
	flag.Float64Var(&certRotationThreshold, "cert-rotation-threshold", 0.8, "Certificate rotation threshold (0-1)")
	flag.StringVar(&targetKubeconfig, "target-kubeconfig", "", "Path to target cluster kubeconfig")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", true, "Enable leader election for HA")
	flag.StringVar(&leaderElectionID, "leader-election-id", "webhook-injector-leader", "Leader election lease name")
	flag.DurationVar(&syncPeriod, "sync-period", 5*time.Minute, "Controller reconciliation sync period")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "Metrics server bind address")
	flag.StringVar(&healthAddr, "health-addr", ":8081", "Health probe server bind address")

	// Initialize scheme
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := admissionv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

func main() {
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Validate required flags
	if webhookConfigName == "" {
		setupLog.Error(nil, "webhook-config-name is required")
		flag.Usage()
		os.Exit(1)
	}

	// Auto-detect namespace if not provided
	if namespace == "" {
		var err error
		namespace, err = detectNamespace()
		if err != nil {
			setupLog.Error(err, "Failed to detect namespace")
			os.Exit(1)
		}
	}

	// Validate cert validity
	if certValidity < time.Minute {
		setupLog.Error(nil, "cert-validity too short, minimum 1m")
		os.Exit(1)
	}

	setupLog.Info("Starting webhook-injector",
		"namespace", namespace,
		"webhook-config-name", webhookConfigName,
		"cert-secret-name", certSecretName,
		"enable-leader-election", enableLeaderElection,
	)

	// Create manager for hosting cluster
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress:  healthAddr,
		LeaderElection:          enableLeaderElection,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: namespace,
		// Cache configuration: Restrict manager to watch only the pod's namespace.
		// This prevents cluster-wide RBAC requirements - the ServiceAccount only needs
		// namespace-scoped permissions for ConfigMaps and Secrets. Without this, the
		// manager would attempt to watch at cluster scope and fail with RBAC errors
		// ("cannot list resource at the cluster scope").
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				namespace: {},
			},
			SyncPeriod: &syncPeriod,
		},
	})
	if err != nil {
		setupLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	// Get target cluster client
	targetClient, err := getTargetClient()
	if err != nil {
		setupLog.Error(err, "Unable to create target cluster client")
		os.Exit(1)
	}

	// LEADER ELECTION ARCHITECTURE:
	//
	// This application uses controller-runtime's leader election mechanism to ensure
	// only one replica performs certificate generation and webhook management, while
	// allowing all replicas to reload certificates to their local filesystem.
	//
	// How it works:
	// 1. Manager is configured with LeaderElection: true (see ctrl.Options above)
	// 2. When multiple replicas start, they compete for a Kubernetes Lease
	// 3. The winner becomes "leader" and runs ALL controllers by default
	// 4. EXCEPT: Controllers configured with WithOptions(controller.Options{NeedLeaderElection: ptr.To(false)})
	//    run on ALL replicas regardless of leader election status
	//
	// Controller behavior:
	// - CertificateReloader: Uses WithOptions with NeedLeaderElection=false
	//   → Runs on ALL replicas (leader + followers)
	//   → Each replica syncs certificates to its own filesystem
	//   → Enables webhook servers on all replicas to reload certificates
	//
	// - WebhookReconciler: Uses default leader election behavior
	//   → Runs ONLY on the leader replica
	//   → Prevents multiple replicas from generating different certificates
	//   → Prevents race conditions when applying webhooks to target cluster
	//
	// Example with 3 replicas:
	//   Replica 1 (Leader):    CertificateReloader ✓  WebhookReconciler ✓
	//   Replica 2 (Follower):  CertificateReloader ✓  WebhookReconciler ✗
	//   Replica 3 (Follower):  CertificateReloader ✓  WebhookReconciler ✗

	// Set up certificate reloader (runs on ALL replicas via WithOptions)
	certReloader := &controllers.CertificateReloader{
		Client:         mgr.GetClient(),
		Namespace:      namespace,
		CertSecretName: certSecretName,
		CertDir:        certDir,
	}
	if err := certReloader.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create CertificateReloader controller")
		os.Exit(1)
	}
	setupLog.Info("CertificateReloader controller registered (runs on all replicas)")

	// Set up webhook reconciler (generates certificates and applies webhooks)
	// Runs on leader ONLY via default leader election behavior to prevent conflicts
	webhookReconciler := &controllers.WebhookReconciler{
		Client:                mgr.GetClient(),
		TargetClient:          targetClient,
		WebhookConfigName:     webhookConfigName,
		Namespace:             namespace,
		CertSecretName:        certSecretName,
		CertValidity:          certValidity,
		CertRotationThreshold: certRotationThreshold,
		Scheme:                mgr.GetScheme(),
	}
	if err := webhookReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create WebhookReconciler controller")
		os.Exit(1)
	}
	setupLog.Info("WebhookReconciler controller registered (runs on leader only)")

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}

func detectNamespace() (string, error) {
	// Priority order: NAMESPACE env var → serviceaccount file → fail
	if ns := os.Getenv("NAMESPACE"); ns != "" {
		return ns, nil
	}

	const serviceAccountFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	data, err := os.ReadFile(serviceAccountFile)
	if err == nil && len(data) > 0 {
		return string(data), nil
	}

	return "", errors.New("namespace not specified via --namespace flag, NAMESPACE env var not set, and serviceaccount namespace file not found")
}

// getTargetClient creates a client for the target cluster
func getTargetClient() (client.Client, error) {
	var targetConfig *rest.Config
	var err error

	if targetKubeconfig != "" {
		// Load from explicit kubeconfig
		targetConfig, err = clientcmd.BuildConfigFromFlags("", targetKubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig from %s: %w", targetKubeconfig, err)
		}
		setupLog.Info("Using explicit kubeconfig for target cluster", "path", targetKubeconfig)
	} else {
		// Use in-cluster config
		targetConfig, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
		}
		setupLog.Info("Using in-cluster config for target cluster")
	}

	targetClient, err := client.New(targetConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create target cluster client: %w", err)
	}

	return targetClient, nil
}
