// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"

	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

var (
	webhookConfigName     string
	managedResourceLabel  string
	targetLabel           string
	certSANs              string
	externalHost          string
	externalPort          int
	namespace             string
	certSecretName        string
	certDir               string
	certValidity          time.Duration
	certRotationThreshold float64
	rotationOverlapWindow time.Duration
	rotationGateTimeout   time.Duration
	targetKubeconfig      string
	enableLeaderElection  bool
	leaderElectionID      string
	syncPeriod            time.Duration
	metricsAddr           string
	healthAddr            string
	forceReconcile        bool
	showVersion           bool
)

func init() {
	// Initialize flag variables
	flag.StringVar(&webhookConfigName, "webhook-config-name", "", "Required. ConfigMap (in --namespace, hosting cluster) carrying a `webhooks.yaml` key whose value contains the MutatingWebhookConfiguration / ValidatingWebhookConfiguration YAML to apply to the target cluster. webhook-injector parses, injects the caBundle, and applies these to the target cluster. CRDs are not handled here — CRDs flow through Gardener ManagedResources, see --managed-resource-label.")
	flag.StringVar(&managedResourceLabel, "managed-resource-label", "", "Label selector (key=value) used to scope the Gardener ManagedResource reconciler on the *hosting* cluster. When set, the controller watches labeled ManagedResources, walks their referenced Secrets, finds embedded CRDs with conversion.strategy=Webhook, and stamps caBundle / rewrites Service-form clientConfig to URL-form using --external-host and --external-port. When unset, the ManagedResource reconciler is not registered.")
	flag.StringVar(&targetLabel, "target-label", "", "Label selector (key=value) enabling target-cluster patch mode. When set, watches CustomResourceDefinitions, MutatingWebhookConfigurations, and ValidatingWebhookConfigurations on the TARGET cluster carrying this label and keeps their caBundle in sync as certs rotate — for GRM-free / direct-apply topologies where these objects are applied directly to the shoot (no ManagedResource). The same label must be present on every object (CRD/MWC/VWC) you want stamped. Only pre-existing labeled objects are stamped; nothing is created or deleted, and only caBundle is patched. Independent of --managed-resource-label. When unset, patch mode is not registered.")
	flag.StringVar(&certSANs, "cert-sans", "", "Comma-separated DNS names and/or IP addresses to include as additional certificate SANs (merged with SANs derived from the ConfigMap's webhook services/URLs).")
	flag.StringVar(&externalHost, "external-host", "", "External host (DNS name or IP, no port) the target cluster uses to reach the labeled webhook callbacks. Required when --managed-resource-label is set. Combined with --external-port to rewrite Service-form conversion clientConfig inside ManagedResource-embedded CRDs to clientConfig.URL = \"https://<external-host>:<external-port><Service.Path>\" (path defaults to \"/\"). Ignored when --managed-resource-label is unset.")
	flag.IntVar(&externalPort, "external-port", 0, "External port the target cluster uses to reach the labeled webhook callbacks. Required when --managed-resource-label is set.")
	flag.StringVar(&namespace, "namespace", "", "Namespace for all resources (auto-detects from pod if not set)")
	flag.StringVar(&certSecretName, "cert-secret-name", "webhook-injector-certs", "Secret name for storing certificates")
	flag.StringVar(&certDir, "cert-dir", "/tmp/webhook-certs", "Directory to write certificate files")
	flag.DurationVar(&certValidity, "cert-validity", 90*24*time.Hour, "Certificate validity duration")
	flag.Float64Var(&certRotationThreshold, "cert-rotation-threshold", 0.2, "Certificate rotation threshold (0-1), rotate when remaining validity falls below this fraction")
	flag.DurationVar(&rotationOverlapWindow, "rotation-overlap-window", 5*time.Minute,
		"Grace duration after a new cert is promoted to serving before the old cert is pruned from the trust bundle. Must be long enough for in-flight TLS connections to drain.")
	flag.DurationVar(&rotationGateTimeout, "rotation-gate-timeout", 2*time.Minute,
		"Maximum duration to wait for shoot CRDs to show the staged union bundle before promoting the new serving cert anyway. The controller would rather have a working webhook than a stuck rotation. The gated CRD set is auto-discovered from ManagedResourceReconciler's most recent reconcile pass (see pkg/controllers/rotationgate); an empty snapshot collapses the gate to this timeout alone.")
	flag.StringVar(&targetKubeconfig, "target-kubeconfig", "", "Path to target cluster kubeconfig")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", true, "Enable leader election for HA")
	flag.StringVar(&leaderElectionID, "leader-election-id", "webhook-injector-leader", "Leader election lease name")
	flag.DurationVar(&syncPeriod, "sync-period", 5*time.Minute, "Controller reconciliation sync period")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8082", "Metrics server bind address")
	flag.StringVar(&healthAddr, "health-addr", ":8083", "Health probe server bind address")
	flag.BoolVar(&forceReconcile, "force-reconcile", true,
		"When --managed-resource-label is set, patch gardener.cloud/operation=reconcile on each ManagedResource after a successful Secret stamp. This kicks gardener-resource-manager out of any backoff so the new caBundle reaches the shoot promptly. The annotation is set only on reconciles that actually wrote a Secret — no-op reconciles do not churn GRM. Default true; set to false to rely solely on GRM's own Secret watch.")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")

	// Initialize scheme
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := admissionv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := resourcesv1alpha1.AddToScheme(scheme); err != nil {
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

	if showVersion {
		fmt.Println("webhook-injector")
		return
	}

	mrLabelKey, mrLabelValue, mrLabelOK := parseLabel(managedResourceLabel)
	if managedResourceLabel != "" && !mrLabelOK {
		setupLog.Error(nil, fmt.Sprintf("--managed-resource-label must be in key=value form, got %q", managedResourceLabel))
		os.Exit(1)
	}

	targetLabelKey, targetLabelValue, targetLabelOK := parseLabel(targetLabel)
	if targetLabel != "" && !targetLabelOK {
		setupLog.Error(nil, fmt.Sprintf("--target-label must be in key=value form, got %q", targetLabel))
		os.Exit(1)
	}

	certSANsList := splitAndTrim(certSANs)

	// At least one mode must be active. ConfigMap mode (--webhook-config-name)
	// and target patch mode (--target-label) are independent; a controller
	// with neither manages nothing and is a misconfiguration.
	if webhookConfigName == "" && !targetLabelOK {
		setupLog.Error(nil, "at least one of --webhook-config-name or --target-label must be set")
		flag.Usage()
		os.Exit(1)
	}

	// Pure patch mode (no ConfigMap) has no webhook services to derive SANs
	// from — --cert-sans is the only SAN source, so it must be non-empty.
	if webhookConfigName == "" && len(certSANsList) == 0 {
		setupLog.Error(nil, "--cert-sans is required when running patch mode without --webhook-config-name")
		os.Exit(1)
	}

	// Shared rotation-gate snapshot bridging ManagedResourceReconciler
	// (publisher) and WebhookReconciler (consumer). Both reconcilers
	// hold the same *Gate; the MR reconciler calls Publish/Forget at
	// stamp time, the WebhookReconciler calls Snapshot during the
	// staged-rotation propagation check. When --managed-resource-label
	// is unset (pure-ConfigMap deployments) the MR reconciler isn't
	// registered and nobody publishes — the Snapshot stays empty and
	// the gate collapses to the time-based --rotation-gate-timeout
	// fallback, which is correct for those deployments (no GRM hop to
	// wait on).
	gate := rotationgate.New()

	// When --managed-resource-label is set the MR reconciler is wired and
	// needs --external-host / --external-port to rewrite Service-form
	// conversion clientConfig inside CRDs embedded in MR Secrets. Without the
	// label the flags are meaningless — log and ignore so misconfiguration is
	// loud but not fatal.
	effectiveExternalHost := ""
	switch {
	case mrLabelOK:
		if externalHost == "" {
			setupLog.Error(nil, "--external-host is required when --managed-resource-label is set")
			os.Exit(1)
		}
		if externalPort < 1 || externalPort > 65535 {
			setupLog.Error(nil, "--external-port is required when --managed-resource-label is set and must be in [1, 65535]", "value", externalPort)
			os.Exit(1)
		}
		effectiveExternalHost = net.JoinHostPort(externalHost, strconv.Itoa(externalPort))
	case externalHost != "" || externalPort != 0:
		setupLog.Info("--external-host / --external-port are ignored without --managed-resource-label",
			"external-host", externalHost, "external-port", externalPort)
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
		"managed-resource-label", managedResourceLabel,
		"target-label", targetLabel,
		"cert-sans", certSANsList,
		"external-host", effectiveExternalHost,
		"cert-secret-name", certSecretName,
		"enable-leader-election", enableLeaderElection,
		"force-reconcile", forceReconcile,
	)

	// Create manager for hosting cluster
	mgrOpts := ctrl.Options{
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
	}
	// When the MR reconciler is wired, override the per-type cache config for
	// ManagedResource only — every other type stays scoped to the pod's
	// namespace via DefaultNamespaces. An *empty* Namespaces map (not nil)
	// is required: per controller-runtime docs a nil map inherits
	// DefaultNamespaces, while an empty map disables that defaulting and
	// caches across all namespaces.
	if mrLabelOK {
		labelSelector := labels.SelectorFromSet(labels.Set{mrLabelKey: mrLabelValue})
		if mgrOpts.Cache.ByObject == nil {
			mgrOpts.Cache.ByObject = map[client.Object]cache.ByObject{}
		}
		mgrOpts.Cache.ByObject[&resourcesv1alpha1.ManagedResource{}] = cache.ByObject{
			Label:      labelSelector,
			Namespaces: map[string]cache.Config{},
		}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
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
	// - WebhookReconciler / ManagedResourceReconciler: default leader-only
	//   → Run ONLY on the leader replica
	//   → Prevents multiple replicas from generating different certificates
	//   → Prevents race conditions when applying webhooks to target cluster
	//
	// Example with 3 replicas:
	//   Replica 1 (Leader):    CertificateReloader ✓  WebhookReconciler ✓  ManagedResourceReconciler ✓
	//   Replica 2 (Follower):  CertificateReloader ✓  WebhookReconciler ✗  ManagedResourceReconciler ✗
	//   Replica 3 (Follower):  CertificateReloader ✓  WebhookReconciler ✗  ManagedResourceReconciler ✗

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

	// rotationTriggers feed the stamping reconcilers when wired.
	// WebhookReconciler signals each non-blocking on every detected
	// rotation; each consumer reads its channel and force-stamps every
	// labeled object with the new caBundle, instead of waiting for the
	// periodic --sync-period tick. Buffered 1 + non-blocking send means a
	// missed signal is harmless — a single signal triggers a full re-stamp
	// pass. One channel per registered stamping reconciler.
	//
	// Triggers are allocated up-front — before webhookReconciler.SetupWithManager
	// runs — so RotationTriggers is populated at Setup time rather than mutated
	// after. The current controller-runtime shape doesn't run Reconcile until
	// mgr.Start, but the earlier post-Setup assignment read as a latent
	// ordering hazard that would bite if anyone moved a Start earlier.
	var rotationTriggers []chan<- struct{}
	var mrTrigger chan struct{}
	if mrLabelOK {
		mrTrigger = make(chan struct{}, 1)
		rotationTriggers = append(rotationTriggers, mrTrigger)
	}
	var targetTrigger chan struct{}
	if targetLabelOK {
		targetTrigger = make(chan struct{}, 1)
		rotationTriggers = append(rotationTriggers, targetTrigger)
	}

	webhookReconciler := &controllers.WebhookReconciler{
		Client:                mgr.GetClient(),
		TargetClient:          targetClient,
		WebhookConfigName:     webhookConfigName,
		CertSANs:              certSANsList,
		Namespace:             namespace,
		CertSecretName:        certSecretName,
		CertValidity:          certValidity,
		CertRotationThreshold: certRotationThreshold,
		Gate:                  gate,
		RotationOverlapWindow: rotationOverlapWindow,
		RotationGateTimeout:   rotationGateTimeout,
		RotationTriggers:      rotationTriggers,
		Scheme:                mgr.GetScheme(),
	}
	if err := webhookReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Unable to create WebhookReconciler controller")
		os.Exit(1)
	}
	setupLog.Info("WebhookReconciler controller registered (runs on leader only)")

	// Optionally reconcile Gardener ManagedResource objects on the hosting
	// cluster so embedded CRDs' clientConfig is stamped at the source,
	// surviving GRM's next sync into the shoot.
	if mrLabelOK {
		mrReconciler := &controllers.ManagedResourceReconciler{
			HostingClient:     mgr.GetClient(),
			CertSecretReader:  mgr.GetAPIReader(),
			Namespace:         namespace,
			CertSecretName:    certSecretName,
			WebhookLabelKey:   mrLabelKey,
			WebhookLabelValue: mrLabelValue,
			ExternalHost:      effectiveExternalHost,
			ForceReconcile:    forceReconcile,
			Gate:              gate,
		}
		if err := mrReconciler.SetupWithManager(mgr, mrTrigger); err != nil {
			setupLog.Error(err, "Unable to create ManagedResourceReconciler controller")
			os.Exit(1)
		}
		setupLog.Info("ManagedResourceReconciler controller registered (runs on leader only)",
			"label", mrLabelKey+"="+mrLabelValue,
			"force-reconcile", forceReconcile,
		)
	}

	// Optionally keep caBundle in sync on CRD/MWC/VWC applied directly to the
	// target cluster (GRM-free patch mode). Opt-in via --target-label. Uses
	// a second, label-scoped cluster.Cluster handle so the target objects are
	// watched from the target cluster's own cache.
	if targetLabelOK {
		targetCfg, err := getTargetRestConfig()
		if err != nil {
			setupLog.Error(err, "Unable to get target cluster config for patch mode")
			os.Exit(1)
		}
		targetSelector := labels.SelectorFromSet(labels.Set{targetLabelKey: targetLabelValue})
		targetCluster, err := cluster.New(targetCfg, func(o *cluster.Options) {
			o.Scheme = scheme
			o.Cache.SyncPeriod = &syncPeriod
			o.Cache.ByObject = map[client.Object]cache.ByObject{
				&apiextensionsv1.CustomResourceDefinition{}:   {Label: targetSelector},
				&admissionv1.MutatingWebhookConfiguration{}:   {Label: targetSelector},
				&admissionv1.ValidatingWebhookConfiguration{}: {Label: targetSelector},
			}
		})
		if err != nil {
			setupLog.Error(err, "Unable to create target cluster handle for patch mode")
			os.Exit(1)
		}
		if err := mgr.Add(&controllers.LeaderOnlyCluster{Cluster: targetCluster}); err != nil {
			setupLog.Error(err, "Unable to add target cluster to manager")
			os.Exit(1)
		}
		// Get/Patch use an uncached client so each reconcile live-reads the
		// target object before patching. The targetCluster handle is used
		// only to drive the label-scoped watches; reading Get through its
		// cache could compute a patch from stale state and clobber another
		// direct-apply owner's fresh changes.
		targetPatchClient, err := client.New(targetCfg, client.Options{Scheme: scheme})
		if err != nil {
			setupLog.Error(err, "Unable to create uncached target client for patch mode")
			os.Exit(1)
		}
		targetReconciler := &controllers.TargetWebhookReconciler{
			TargetClient:     targetPatchClient,
			CertSecretReader: mgr.GetAPIReader(),
			Namespace:        namespace,
			CertSecretName:   certSecretName,
			LabelKey:         targetLabelKey,
			LabelValue:       targetLabelValue,
			Gate:             gate,
		}
		if err := targetReconciler.SetupWithManager(mgr, targetCluster, targetTrigger); err != nil {
			setupLog.Error(err, "Unable to create TargetWebhookReconciler controller")
			os.Exit(1)
		}
		setupLog.Info("TargetWebhookReconciler controller registered (patch mode, cache + controllers leader-only)",
			"label", targetLabelKey+"="+targetLabelValue,
		)
	}

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", certReadyCheck(certDir)); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}

func certReadyCheck(dir string) healthz.Checker {
	return func(_ *http.Request) error {
		for _, name := range []string{"tls.crt", "tls.key"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				return fmt.Errorf("certificate file %s not ready: %w", name, err)
			}
		}
		return nil
	}
}

// splitAndTrim splits a comma-separated list and drops empty entries.
func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseLabel parses a "key=value" label string. Returns ok=false when the input
// is empty, lacks an "=", or has an empty key/value after trimming.
func parseLabel(s string) (key, value string, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", false
	}
	k, v, found := strings.Cut(s, "=")
	if !found {
		return "", "", false
	}
	key = strings.TrimSpace(k)
	value = strings.TrimSpace(v)
	if key == "" || value == "" {
		return "", "", false
	}
	return key, value, true
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

// getTargetRestConfig returns the rest.Config for the target cluster.
func getTargetRestConfig() (*rest.Config, error) {
	if targetKubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", targetKubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig from %s: %w", targetKubeconfig, err)
		}
		setupLog.Info("Using explicit kubeconfig for target cluster", "path", targetKubeconfig)
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}
	setupLog.Info("Using in-cluster config for target cluster")
	return cfg, nil
}

// getTargetClient returns an uncached client for the target cluster. ConfigMap
// mode uses this to apply user-authored MWC/VWC directly without watching the
// target cluster's webhook objects.
func getTargetClient() (client.Client, error) {
	cfg, err := getTargetRestConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create target cluster client: %w", err)
	}
	return c, nil
}
