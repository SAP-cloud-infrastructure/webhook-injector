// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
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
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	whadmission "github.com/sap-cloud-infrastructure/webhook-injector/pkg/admission"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/certificates"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

var (
	webhookConfigName     string
	webhookLabel          string
	certSANs              string
	externalHost          string
	externalPort          int
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
	admissionWebhookName  string
	admissionWebhookPort  int
	admissionExternalPort int
	showVersion           bool
)

func init() {
	// Initialize flag variables
	flag.StringVar(&webhookConfigName, "webhook-config-name", "", "ConfigMap name containing webhook definitions (ConfigMap mode). Mutually exclusive with --webhook-label.")
	flag.StringVar(&webhookLabel, "webhook-label", "", "Label selector (key=value) used to discover MutatingWebhookConfigurations, ValidatingWebhookConfigurations, and CustomResourceDefinitions in the target cluster whose caBundle should be patched (patch mode). Required in patch mode.")
	flag.StringVar(&certSANs, "cert-sans", "", "Comma-separated DNS names and/or IP addresses to include as certificate SANs. Required in patch mode. Optional in ConfigMap mode (in addition to SANs derived from the ConfigMap).")
	flag.StringVar(&externalHost, "external-host", "", "External host (DNS name or IP, no port) the target cluster uses to reach this controller. Required in patch mode. Combined with --external-port to rewrite labeled MWC/VWC/CRD clientConfig.Service entries to clientConfig.URL = \"https://<external-host>:<external-port><Service.Path>\" (path defaults to \"/\"). Combined with --admission-external-port to form the bootstrap MWC URL. caBundle handling is unchanged. Ignored in ConfigMap mode.")
	flag.IntVar(&externalPort, "external-port", 0, "External port the target cluster uses to reach the labeled MWC/VWC/CRD webhook callbacks. Required in patch mode. Distinct from --admission-external-port, which targets this controller's own admission endpoint.")
	flag.StringVar(&namespace, "namespace", "", "Namespace for all resources (auto-detects from pod if not set)")
	flag.StringVar(&certSecretName, "cert-secret-name", "webhook-injector-certs", "Secret name for storing certificates")
	flag.StringVar(&certDir, "cert-dir", "/tmp/webhook-certs", "Directory to write certificate files")
	flag.DurationVar(&certValidity, "cert-validity", 90*24*time.Hour, "Certificate validity duration")
	flag.Float64Var(&certRotationThreshold, "cert-rotation-threshold", 0.2, "Certificate rotation threshold (0-1), rotate when remaining validity falls below this fraction")
	flag.StringVar(&targetKubeconfig, "target-kubeconfig", "", "Path to target cluster kubeconfig")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", true, "Enable leader election for HA")
	flag.StringVar(&leaderElectionID, "leader-election-id", "webhook-injector-leader", "Leader election lease name")
	flag.DurationVar(&syncPeriod, "sync-period", 5*time.Minute, "Controller reconciliation sync period")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8082", "Metrics server bind address")
	flag.StringVar(&healthAddr, "health-addr", ":8083", "Health probe server bind address")
	flag.StringVar(&admissionWebhookName, "admission-webhook-name", "webhook-injector-mutator", "Name of the MutatingWebhookConfiguration the controller bootstraps in the target cluster to register its own admission mutator. Patch mode only. Empty disables admission-webhook bootstrap.")
	flag.IntVar(&admissionWebhookPort, "admission-webhook-port", 9444, "Port the admission webhook server binds to inside the pod. Patch mode only. Configure your Service/NodePort/LoadBalancer to forward --admission-external-port to this port.")
	flag.IntVar(&admissionExternalPort, "admission-external-port", 0, "External port (combined with --external-host) the target cluster uses to reach the admission webhook server. Required in patch mode when admission-webhook bootstrap is active. Distinct from --external-port, which targets the labeled webhook callbacks rather than this controller's admission endpoint.")
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

	// Validate mode selection. ConfigMap mode and patch mode are mutually exclusive,
	// but ConfigMap mode may also accept --webhook-label to additionally refresh
	// caBundle on labeled CRDs (labeled admission webhooks are ignored in that case).
	labelKey, labelValue, labelOK := parseLabel(webhookLabel)
	certSANsList := splitAndTrim(certSANs)
	configMapModeActive := webhookConfigName != ""
	patchModeActive := !configMapModeActive && labelOK

	if webhookLabel != "" && !labelOK {
		setupLog.Error(nil, fmt.Sprintf("--webhook-label must be in key=value form, got %q", webhookLabel))
		os.Exit(1)
	}
	if !configMapModeActive && !patchModeActive {
		setupLog.Error(nil, "either --webhook-config-name or --webhook-label must be set")
		flag.Usage()
		os.Exit(1)
	}
	if patchModeActive && len(certSANsList) == 0 {
		setupLog.Error(nil, "--cert-sans is required in patch mode")
		os.Exit(1)
	}
	if patchModeActive && externalHost == "" {
		setupLog.Error(nil, "--external-host is required in patch mode")
		os.Exit(1)
	}
	if patchModeActive && (externalPort < 1 || externalPort > 65535) {
		setupLog.Error(nil, "--external-port is required in patch mode and must be in [1, 65535]", "value", externalPort)
		os.Exit(1)
	}
	// --admission-external-port is required when bootstrap is active so the
	// MWC URL we install in the target cluster points at a port the user has
	// actually mapped to --admission-webhook-port. We don't pick a default
	// because the value depends entirely on the user's Service / NodePort /
	// LoadBalancer wiring, and a wrong default silently produces a bootstrap
	// MWC the apiserver cannot reach.
	if patchModeActive && admissionWebhookName != "" {
		if admissionExternalPort < 1 || admissionExternalPort > 65535 {
			setupLog.Error(nil, "--admission-external-port is required in patch mode (when admission-webhook bootstrap is active) and must be in [1, 65535]",
				"value", admissionExternalPort)
			os.Exit(1)
		}
	}

	// Compose the labeled-webhook callback host:port from --external-host +
	// --external-port. Patch mode only; in ConfigMap mode --external-host is
	// silently ignored after a warning so users running mixed deployments
	// don't have to template flags differently per cluster.
	effectiveExternalHost := ""
	if patchModeActive {
		effectiveExternalHost = net.JoinHostPort(externalHost, strconv.Itoa(externalPort))
	} else if externalHost != "" {
		setupLog.Info("--external-host is ignored in ConfigMap mode", "external-host", externalHost)
	}

	// Compose the host:port the bootstrap MWC will use. Reuse --external-host
	// (so users only configure DNS/IP once) but combine it with
	// --admission-external-port, since that targets this controller's
	// admission server, not the labeled webhook callbacks.
	effectiveAdmissionExternalHost := ""
	if patchModeActive && admissionWebhookName != "" {
		effectiveAdmissionExternalHost = net.JoinHostPort(externalHost, strconv.Itoa(admissionExternalPort))
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
		"mode", modeLabel(configMapModeActive),
		"webhook-config-name", webhookConfigName,
		"webhook-label", webhookLabel,
		"cert-sans", certSANsList,
		"external-host", effectiveExternalHost,
		"admission-external-host", effectiveAdmissionExternalHost,
		"cert-secret-name", certSecretName,
		"enable-leader-election", enableLeaderElection,
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
	if patchModeActive {
		mgrOpts.WebhookServer = webhook.NewServer(webhook.Options{
			Port:    admissionWebhookPort,
			CertDir: certDir,
		})
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
		WebhookLabelKey:       labelKey,
		WebhookLabelValue:     labelValue,
		CertSANs:              certSANsList,
		ExternalHost:          effectiveExternalHost,
		AdmissionWebhookName:  effectiveAdmissionWebhookName(patchModeActive),
		AdmissionExternalHost: effectiveAdmissionExternalHost,
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

	// Register the admission mutator handlers in patch mode. Runs on all replicas
	// (controller-runtime's webhook server does not participate in leader election),
	// matching CertificateReloader's behavior.
	if patchModeActive {
		mutator := &whadmission.Mutator{
			Decoder:      admission.NewDecoder(mgr.GetScheme()),
			ExternalHost: effectiveExternalHost,
			CABundle:     newCABundleSource(mgr.GetClient(), namespace, certSecretName),
			SelfMWCName:  effectiveAdmissionWebhookName(patchModeActive),
		}
		ws := mgr.GetWebhookServer()
		ws.Register("/mutate-mwc", &webhook.Admission{Handler: mutator})
		ws.Register("/mutate-vwc", &webhook.Admission{Handler: mutator})
		ws.Register("/mutate-crd", &webhook.Admission{Handler: mutator})
		setupLog.Info("Admission mutator registered",
			"port", admissionWebhookPort, "certDir", certDir,
			"paths", []string{"/mutate-mwc", "/mutate-vwc", "/mutate-crd"},
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

	// In patch mode the manager hosts a webhook server whose certwatcher reads
	// tls.crt/tls.key from --cert-dir at mgr.Start. On a cold start neither the
	// Secret nor the on-disk files exist yet, and the controller that would
	// create them cannot run until mgr.Start succeeds — chicken and egg.
	// Resolve it here, outside the manager, so every replica races to populate
	// the Secret atomically (one wins via Create; losers re-Get the winner's
	// data) and writes the resulting cert/key to disk before mgr.Start.
	if patchModeActive {
		if err := bootstrapPatchModeCerts(context.Background(), namespace, certSecretName, certDir, certSANsList, certValidity); err != nil {
			setupLog.Error(err, "Failed to bootstrap certificate files for webhook server")
			os.Exit(1)
		}
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

func modeLabel(configMapMode bool) string {
	if configMapMode {
		return "configmap"
	}
	return "patch"
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

// effectiveAdmissionWebhookName returns the configured admission-webhook name
// only when patch mode is active; ConfigMap mode does not bootstrap an
// admission webhook.
func effectiveAdmissionWebhookName(patchMode bool) string {
	if !patchMode {
		return ""
	}
	return admissionWebhookName
}

// newCABundleSource returns a callback the admission mutator invokes on every
// request to read the current PEM cert. It uses the manager's cache reader,
// which is populated by CertificateReloader's watch on the certificate Secret,
// so cert rotations propagate without restarting the webhook server.
func newCABundleSource(c client.Client, ns, secretName string) whadmission.CABundleSource {
	const tlsCrtKey = "tls.crt"
	return func(ctx context.Context) ([]byte, error) {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: secretName, Namespace: ns}, secret); err != nil {
			return nil, err
		}
		return secret.Data[tlsCrtKey], nil
	}
}

// bootstrapPatchModeCerts ensures tls.crt/tls.key exist on disk before
// mgr.Start. The webhook server's certwatcher fails fast if either file is
// missing, so this preflight runs *outside* the manager — it cannot wait for a
// reconciler that hasn't started yet.
//
// Every replica races: each tries to read the cert Secret, and if it does not
// exist, generates a fresh self-signed cert and Creates the Secret. Atomicity
// of Create means exactly one replica wins; the rest get AlreadyExists and
// re-read whatever the winner stored. All replicas then write the same bytes
// to --cert-dir.
//
// The leader still owns rotation post-startup via WebhookReconciler. This
// function only handles the initial population.
func bootstrapPatchModeCerts(ctx context.Context, namespace, secretName, certDir string, certSANs []string, validity time.Duration) error {
	const (
		tlsCrtKey = "tls.crt"
		tlsKeyKey = "tls.key"
	)

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("get rest config: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("build hosting-cluster client: %w", err)
	}

	key := types.NamespacedName{Name: secretName, Namespace: namespace}
	secret := &corev1.Secret{}
	getErr := c.Get(ctx, key, secret)
	switch {
	case getErr == nil:
		// Existing Secret — use whatever is already there. If a field is
		// missing or empty the WebhookReconciler will rotate post-startup;
		// the certwatcher just needs *some* parsable file to come up.
	case apierrors.IsNotFound(getErr):
		services := make([]certificates.ServiceReference, 0, len(certSANs))
		for _, san := range certSANs {
			services = append(services, certificates.ServiceReference{Hostname: san})
		}
		certPEM, keyPEM, genErr := certificates.GenerateSelfSignedCert(services, validity)
		if genErr != nil {
			return fmt.Errorf("generate bootstrap cert: %w", genErr)
		}
		fresh := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
			Type:       corev1.SecretTypeTLS,
			Data: map[string][]byte{
				tlsCrtKey: certPEM,
				tlsKeyKey: keyPEM,
			},
		}
		createErr := c.Create(ctx, fresh)
		switch {
		case createErr == nil:
			secret = fresh
			setupLog.Info("Bootstrapped certificate Secret", "name", secretName, "namespace", namespace)
		case apierrors.IsAlreadyExists(createErr):
			if err := c.Get(ctx, key, secret); err != nil {
				return fmt.Errorf("re-read Secret after AlreadyExists: %w", err)
			}
		default:
			return fmt.Errorf("create bootstrap Secret: %w", createErr)
		}
	default:
		return fmt.Errorf("read cert Secret: %w", getErr)
	}

	certPEM := secret.Data[tlsCrtKey]
	keyPEM := secret.Data[tlsKeyKey]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return fmt.Errorf("secret %s/%s missing %s or %s", namespace, secretName, tlsCrtKey, tlsKeyKey)
	}

	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return fmt.Errorf("create cert dir %s: %w", certDir, err)
	}
	if err := os.WriteFile(filepath.Join(certDir, tlsCrtKey), certPEM, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tlsCrtKey, err)
	}
	if err := os.WriteFile(filepath.Join(certDir, tlsKeyKey), keyPEM, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tlsKeyKey, err)
	}
	setupLog.Info("Wrote bootstrap certificate files", "dir", certDir)
	return nil
}
