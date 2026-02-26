package main

import (
	"flag"
	"fmt"
	"os"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = admissionv1.AddToScheme(scheme)
}

func main() {
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Load configuration
	cfg := config.LoadFromEnv()
	if err := cfg.Validate(); err != nil {
		setupLog.Error(err, "Invalid configuration")
		os.Exit(1)
	}

	setupLog.Info("Starting webhook-injector",
		"config-name", cfg.WebhookConfigName,
		"cert-secret", cfg.CertSecretName,
		"leader-election", cfg.EnableLeaderElection,
	)

	// Create manager for hosting cluster
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: cfg.MetricsAddr,
		},
		HealthProbeBindAddress:  cfg.HealthAddr,
		LeaderElection:          cfg.EnableLeaderElection,
		LeaderElectionID:        cfg.LeaderElectionID,
		LeaderElectionNamespace: cfg.LeaderElectionNamespace,
	})
	if err != nil {
		setupLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	// Get target cluster client
	targetClient, err := getTargetClient(cfg)
	if err != nil {
		setupLog.Error(err, "Unable to create target cluster client")
		os.Exit(1)
	}

	// Set up certificate reloader (runs on all replicas)
	certReloader := &controllers.CertificateReloader{
		Client: mgr.GetClient(),
		Config: cfg,
	}
	if err := certReloader.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create CertificateReloader controller")
		os.Exit(1)
	}
	setupLog.Info("CertificateReloader controller registered")

	// Set up leader reconciler (runs on leader only - handled by leader election)
	leaderReconciler := &controllers.LeaderReconciler{
		Client:       mgr.GetClient(),
		TargetClient: targetClient,
		Config:       cfg,
		Scheme:       mgr.GetScheme(),
	}
	if err := leaderReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create LeaderReconciler controller")
		os.Exit(1)
	}
	setupLog.Info("LeaderReconciler controller registered")

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

// getTargetClient creates a client for the target cluster
func getTargetClient(cfg *config.Config) (client.Client, error) {
	var targetConfig *rest.Config
	var err error

	if cfg.Kubeconfig != "" {
		// Load from explicit kubeconfig
		targetConfig, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig from %s: %w", cfg.Kubeconfig, err)
		}
		setupLog.Info("Using explicit kubeconfig for target cluster", "path", cfg.Kubeconfig)
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
