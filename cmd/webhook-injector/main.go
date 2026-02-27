// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 SAP SE or an SAP affiliate company
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
	"sigs.k8s.io/controller-runtime/pkg/cache"
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

	// Load configuration
	cfg, err := config.LoadFromEnv()
	if err != nil {
		setupLog.Error(err, "failed to load configuration")
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		setupLog.Error(err, "Invalid configuration")
		os.Exit(1)
	}

	setupLog.Info("Starting webhook-injector",
		"config-name", cfg.WebhookConfigNamespace+"/"+cfg.WebhookConfigName,
		"cert-secret", cfg.CertSecretNamespace+"/"+cfg.CertSecretName,
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
		// Cache configuration: Restrict manager to watch only the pod's namespace.
		// This prevents cluster-wide RBAC requirements - the ServiceAccount only needs
		// namespace-scoped permissions for ConfigMaps and Secrets. Without this, the
		// manager would attempt to watch at cluster scope and fail with RBAC errors
		// ("cannot list resource at the cluster scope").
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				getPodNamespace(cfg): {},
			},
		},
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
		Client: mgr.GetClient(),
		Config: cfg,
	}
	if err := certReloader.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create CertificateReloader controller")
		os.Exit(1)
	}
	setupLog.Info("CertificateReloader controller registered (runs on all replicas)")

	// Set up webhook reconciler (generates certificates and applies webhooks)
	// Runs on leader ONLY via default leader election behavior to prevent conflicts
	webhookReconciler := &controllers.WebhookReconciler{
		Client:       mgr.GetClient(),
		TargetClient: targetClient,
		Config:       cfg,
		Scheme:       mgr.GetScheme(),
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

// getPodNamespace returns the namespace the pod is running in
func getPodNamespace(cfg *config.Config) string {
	// Try config namespaces first
	if cfg.WebhookConfigNamespace != "" {
		return cfg.WebhookConfigNamespace
	}
	if cfg.CertSecretNamespace != "" {
		return cfg.CertSecretNamespace
	}

	// Try to read from pod
	if ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return string(ns)
	}

	return "default"
}
