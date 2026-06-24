// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/certificates"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/managedresource"
)

// loadContextConfig builds a rest.Config from a kubeconfig file, pinned to
// the given context. Used by tests that talk directly to the kind clusters
// via controller-runtime client.
func loadContextConfig(t *testing.T, kubeconfigPath, contextName string) *rest.Config {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		t.Fatalf("Load kubeconfig context %s: %v", contextName, err)
	}
	return cfg
}

// TestManagedResourceFlow exercises ManagedResourceReconciler end-to-end
// against the real kind-hosting apiserver. It:
//
//  1. Installs the Gardener ManagedResource CRD on the hosting cluster
//     (so the apiserver accepts the typed object we create below).
//  2. Creates a Secret holding a multi-doc YAML with one CRD that uses
//     Service-form conversion.strategy=Webhook clientConfig.
//  3. Creates a labeled ManagedResource pointing at that Secret with
//     spec.keepObjects=true.
//  4. Drives the controller's Reconcile() loop directly against the real
//     apiserver (no manager, no leader election, no second deployment).
//     This is faithful to the production path — SetupWithManager only
//     wires watches and predicates; the actual mutation logic is Reconcile.
//  5. Asserts the Secret now embeds a stamped CRD (caBundle = cert Secret's
//     tls.crt, clientConfig.url = https://<external-host><path>, Service
//     cleared) and that gardener.cloud/operation=reconcile was patched onto
//     the MR.
//  6. Re-runs Reconcile to assert idempotency (no extra Secret update, no
//     extra force-reconcile patch).
//  7. Forces a cert rotation via the same annotation TestMetrics uses, then
//     re-runs Reconcile and asserts the embedded CRD's caBundle picked up
//     the new tls.crt.
//  8. Replaces the Secret value with a brotli-compressed key to confirm
//     the .br codec path also stamps.
//
// What this does NOT exercise:
//   - SetupWithManager / cache.ByObject label scoping / the resync trigger
//     manager.Runnable. Those are unit-tested in
//     pkg/controllers/managed_resource_reconciler_test.go against a fake
//     client, and they are pure controller-runtime glue.
//   - A live webhook-injector pod with --managed-resource-label set.
//     Standing up a second deployment alongside the existing skaffold
//     deploy would add ~200 lines of new manifests for one test; the
//     reconciler logic is identical whether driven by a manager or by
//     Reconcile() directly. The cert Secret it reads is created by the
//     existing deployment already running.
//   - Prom metrics over the /metrics HTTP endpoint. Since the reconciler
//     runs in-process here, we read the same counters directly from
//     ctrlmetrics.Registry.
//
// Gating: the test requires kind clusters to be up. It detects whether the
// kind-hosting and kind-target clusters from the shared e2e harness already
// exist and reuses them — this matters for CI where running a fresh setup
// for every test in this package exhausts Docker network/image budget on
// the runner. Only when no clusters exist (e.g. running this test in
// isolation locally) does it invoke ./setup.sh + skaffold + ./teardown.sh.
// When -test.reuse-clusters is set, the same reuse path is taken regardless.
func TestManagedResourceFlow(t *testing.T) {
	kubeconfigPath, absErr := filepath.Abs(filepath.Join(".", "kubeconfig"))
	if absErr != nil {
		t.Fatalf("Resolve kubeconfig path: %v", absErr)
	}
	t.Setenv("KUBECONFIG", kubeconfigPath)

	// Suppress controller-runtime's "log.SetLogger(...) was never called"
	// stack-dump on the first FromContext call. We run Reconcile directly
	// (no manager), so without this the test emits a multi-line stack-trace
	// warning that hides the real test output in CI logs.
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	// Decide cluster lifecycle: if the kind clusters from a previous test in
	// this package (or a manual `make e2e-setup`) already exist, reuse them.
	// This keeps the e2e suite within the Docker/CPU budget on CI runners
	// that have to set up 3+ tests in sequence. Only do a full setup when
	// no clusters exist AND -test.reuse-clusters isn't set.
	clustersExist := kindClustersExist(t)
	if !*reuseClusters && !clustersExist {
		logWithTimestamp(t, "No existing kind clusters detected; setting up...")
		if err := runCommand(t, "./setup.sh"); err != nil {
			t.Fatalf("Setup failed: %v", err)
		}
		if err := runCommand(t, "skaffold run"); err != nil {
			t.Fatalf("Skaffold deployment failed: %v", err)
		}
		t.Cleanup(func() {
			logWithTimestamp(t, "Tearing down clusters...")
			runCommand(t, "./teardown.sh") //nolint:errcheck // cleanup
		})
	} else {
		logWithTimestamp(t, "Reusing existing kind clusters (clustersExist=%v, reuseClusters=%v)",
			clustersExist, *reuseClusters)
	}

	hostingClient := getKubernetesClient(t, "kind-hosting")

	// The cert Secret tls.crt is populated by the running ConfigMap-mode
	// WebhookReconciler. Wait for the deployment so we don't race startup.
	logWithTimestamp(t, "Waiting for webhook deployment ready...")
	waitForDeploymentReady(t, hostingClient, "webhook-server", 3, 5*time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Build a typed controller-runtime client.
	cfg := loadContextConfig(t, kubeconfigPath, "kind-hosting")
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(resourcesv1alpha1.AddToScheme(scheme))
	crClient, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Build controller-runtime client: %v", err)
	}

	// --- 1. Install the Gardener ManagedResource CRD on the hosting cluster.
	installManagedResourceCRD(t, ctx, crClient)
	t.Cleanup(func() {
		// Best-effort: leave the CRD if there are still MRs around. The
		// teardown.sh deletes the whole cluster anyway when not reusing.
		_ = crClient.Delete(context.Background(), &apiextensionsv1.CustomResourceDefinition{ //nolint:errcheck
			ObjectMeta: metav1.ObjectMeta{Name: managedResourceCRDName},
		})
	})

	const (
		ns              = "default"
		mrName          = "mr-e2e-test"
		secretName      = "mr-e2e-test-resources" //nolint:gosec // not a credential, it's the Secret resource name
		webhookLabelKey = "webhook-injector.sap.com/test"
		webhookLabelVal = "true"
		// Must match the external-host shape webhookhost.BuildExternalURL
		// would produce in a real deployment (host:port).
		externalHost     = "webhook-injector-e2e.example.com:9443"
		crdName          = "managedresourcewidgets.example.com"
		convertPath      = "/convert"
		certSecretName   = "webhook-injector-certs"
		controllerCertNs = "default"
	)
	expectedURL := "https://" + externalHost + convertPath

	// Always start clean: a leftover MR/Secret from a previous run would
	// confuse the assertions below.
	cleanupTestObjects(t, ctx, crClient, ns, mrName, secretName)
	t.Cleanup(func() {
		cleanupTestObjects(t, context.Background(), crClient, ns, mrName, secretName)
	})

	// --- 2. Create a Secret holding a multi-doc YAML with the CRD.
	crdYAML := serviceFormCRD(t, crdName, ns, "managedresourcewidget-conversion", convertPath)
	// Multi-doc: a ConfigMap (non-CRD) + the CRD. Tests that the codec only
	// touches the CRD and leaves the ConfigMap byte-for-byte.
	//nolint:gosec // G101 false-positive on the YAML literal below; no credentials.
	const passthroughDoc = `apiVersion: v1
kind: ConfigMap
metadata:
  name: not-a-crd
  namespace: default
data:
  hello: world
`
	combined := bytes.Join([][]byte{[]byte(passthroughDoc), crdYAML}, []byte("---\n"))

	mrSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Data:       map[string][]byte{"objects.yaml": combined},
	}
	if err := crClient.Create(ctx, mrSecret); err != nil {
		t.Fatalf("Create MR resource Secret: %v", err)
	}

	// --- 3. Create the labeled MR pointing at the Secret.
	keep := true
	mr := &resourcesv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mrName,
			Namespace: ns,
			Labels:    map[string]string{webhookLabelKey: webhookLabelVal},
		},
		Spec: resourcesv1alpha1.ManagedResourceSpec{
			SecretRefs:  []corev1.LocalObjectReference{{Name: secretName}},
			KeepObjects: &keep,
		},
	}
	if err := crClient.Create(ctx, mr); err != nil {
		t.Fatalf("Create ManagedResource: %v", err)
	}

	// --- 4. Drive Reconcile() against the real apiserver.
	reconciler := &controllers.ManagedResourceReconciler{
		HostingClient:     crClient,
		Namespace:         controllerCertNs,
		CertSecretName:    certSecretName,
		WebhookLabelKey:   webhookLabelKey,
		WebhookLabelValue: webhookLabelVal,
		ExternalHost:      externalHost,
		ForceReconcile:    true,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: mrName}}

	// Snapshot the metrics counters we care about so we can assert a delta
	// rather than an absolute value — other tests in this binary may have
	// touched them already.
	baselineStampsStamped := readCounterVecValue(t, "webhook_injector_managed_resource_stamps_total", map[string]string{"result": "stamped"})
	baselineCRDsStamped := readCounterValue(t, "webhook_injector_managed_resource_crds_stamped_total")
	baselineForcePatched := readCounterVecValue(t, "webhook_injector_managed_resource_force_reconcile_total", map[string]string{"result": "patched"})

	logWithTimestamp(t, "First Reconcile (expect stamp + force-reconcile patch)...")
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("First Reconcile: %v", err)
	}

	// --- 5. Assert the stamp landed.
	caCert := getCABundle(t, ctx, crClient, controllerCertNs, certSecretName)
	gotSecret := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, gotSecret); err != nil {
		t.Fatalf("Get MR Secret after first reconcile: %v", err)
	}
	stampedCRD := extractCRDFromSecret(t, gotSecret.Data["objects.yaml"], crdName)
	assertCRDStamped(t, stampedCRD, caCert, expectedURL)

	// passthrough doc must still be exact bytes
	if !bytes.Contains(gotSecret.Data["objects.yaml"], []byte(passthroughDoc)) {
		t.Errorf("Non-CRD document was not preserved byte-for-byte; objects.yaml=%s",
			string(gotSecret.Data["objects.yaml"]))
	}

	// Force-reconcile annotation must be set.
	gotMR := &resourcesv1alpha1.ManagedResource{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: mrName}, gotMR); err != nil {
		t.Fatalf("Get MR after first reconcile: %v", err)
	}
	if gotMR.Annotations["gardener.cloud/operation"] != "reconcile" {
		t.Errorf("Expected gardener.cloud/operation=reconcile annotation, got %q",
			gotMR.Annotations["gardener.cloud/operation"])
	}

	// Metrics deltas
	assertDelta(t, "managed_resource_stamps_total{result=stamped}",
		baselineStampsStamped,
		readCounterVecValue(t, "webhook_injector_managed_resource_stamps_total", map[string]string{"result": "stamped"}),
		1)
	assertDelta(t, "managed_resource_crds_stamped_total",
		baselineCRDsStamped,
		readCounterValue(t, "webhook_injector_managed_resource_crds_stamped_total"),
		1)
	assertDelta(t, "managed_resource_force_reconcile_total{result=patched}",
		baselineForcePatched,
		readCounterVecValue(t, "webhook_injector_managed_resource_force_reconcile_total", map[string]string{"result": "patched"}),
		1)

	// --- 6. Idempotency: a second Reconcile must not Update the Secret again.
	// We can observe that indirectly by snapshotting resourceVersion before
	// and after. With the stamp already applied, maybeStampCRDDoc returns
	// changed=false on every doc, so stampSecret short-circuits and no
	// Update is issued.
	beforeRV := gotSecret.ResourceVersion
	beforeStampsConverged := readCounterVecValue(t, "webhook_injector_managed_resource_stamps_total", map[string]string{"result": "converged"})

	logWithTimestamp(t, "Second Reconcile (expect converged, no Secret update)...")
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Second Reconcile: %v", err)
	}

	gotSecret2 := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, gotSecret2); err != nil {
		t.Fatalf("Get MR Secret after second reconcile: %v", err)
	}
	if gotSecret2.ResourceVersion != beforeRV {
		t.Errorf("Second Reconcile bumped resourceVersion (before=%s after=%s) — idempotency broken",
			beforeRV, gotSecret2.ResourceVersion)
	}
	assertDelta(t, "managed_resource_stamps_total{result=converged}",
		beforeStampsConverged,
		readCounterVecValue(t, "webhook_injector_managed_resource_stamps_total", map[string]string{"result": "converged"}),
		1)

	// --- 7. Rotation re-stamp.
	// Trigger a forced cert rotation against the running deployment's Secret,
	// poll for the new tls.crt to differ from caCert, then Reconcile again
	// and confirm the embedded CRD's caBundle picked up the new bytes.
	logWithTimestamp(t, "Triggering cert rotation...")
	if err := triggerRotation(ctx, t, hostingClient); err != nil {
		t.Fatalf("triggerRotation: %v", err)
	}

	newCACert := waitForCABundleChange(t, ctx, crClient, controllerCertNs, certSecretName, caCert, 60*time.Second)
	logWithTimestamp(t, "New cert observed; running Reconcile to re-stamp...")
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile after rotation: %v", err)
	}
	gotSecret3 := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, gotSecret3); err != nil {
		t.Fatalf("Get MR Secret after rotation reconcile: %v", err)
	}
	restamped := extractCRDFromSecret(t, gotSecret3.Data["objects.yaml"], crdName)
	if !bytes.Equal(restamped.Spec.Conversion.Webhook.ClientConfig.CABundle, newCACert) {
		t.Errorf("After rotation: embedded CRD caBundle did not refresh to new tls.crt")
	}

	// --- 7b. Staged-rotation phase machine end-to-end.
	//
	// The deployed WebhookReconciler with the default --rotation-overlap-window=5m
	// stages, propagates (empty gate => instant), and promotes within a single
	// Reconcile pass — so the staged annotation never persists long enough for a
	// poller to observe it. We need to drive the phase machine ourselves with
	// known timing knobs. To avoid the deployed controller racing us on the same
	// Secret, scale webhook-server to 0 replicas for the duration of this block
	// and restore on cleanup.
	logWithTimestamp(t, "Scaling webhook-server deployment to 0 to quiesce deployed reconciler...")
	originalReplicas := scaleDeployment(t, ctx, hostingClient, "default", "webhook-server", 0)
	t.Cleanup(func() {
		logWithTimestamp(t, "Restoring webhook-server replicas to %d...", originalReplicas)
		scaleDeployment(t, context.Background(), hostingClient, "default", "webhook-server", originalReplicas)
	})
	waitForDeploymentScale(t, hostingClient, "webhook-server", 0, 60*time.Second)

	// Build the target-cluster client the in-process WebhookReconciler will
	// use for applyWebhookConfigurations. Reconcile() in this controller calls
	// the target apiserver as part of the tail, but the assertions below only
	// look at the hosting cert Secret; we wire the real target client to keep
	// the call path faithful.
	targetCfg := loadContextConfig(t, kubeconfigPath, "kind-target")
	targetCRClient, err := ctrlclient.New(targetCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Build target controller-runtime client: %v", err)
	}

	// In-process WebhookReconciler with deterministic phase-machine knobs:
	//   - Gate empty => checkPropagation returns true immediately,
	//     so staged → active happens on the next Reconcile pass.
	//   - RotationOverlapWindow stays at the default for stages (b)–(d); we
	//     shrink it just before the prune assertion in (e).
	webhookRec := &controllers.WebhookReconciler{
		Client:                crClient,
		TargetClient:          targetCRClient,
		WebhookConfigName:     "webhook-config",
		Namespace:             "default",
		CertSecretName:        certSecretName,
		CertValidity:          1 * time.Hour,
		CertRotationThreshold: 0.5,
		Scheme:                scheme,
		Gate:                  rotationgate.New(),
	}
	webhookReq := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "default",
		Name:      "webhook-config",
	}}

	// Capture the pre-rotation leaf key. The promote step must change tls.key.
	// We do NOT assert a specific cert count here: the prior rotation block
	// (lines 318-339) drove the deployed reconciler through stage → active,
	// which may leave the bundle with 1 or 2 certs depending on whether the
	// 5m overlap window has elapsed. The phase-machine assertions below
	// validate the transitions we trigger ourselves.
	preStageSecret := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: controllerCertNs, Name: certSecretName}, preStageSecret); err != nil {
		t.Fatalf("Get cert Secret before staged rotation: %v", err)
	}
	preStageKey := append([]byte(nil), preStageSecret.Data["tls.key"]...)

	// (a) Trigger force-rotation again, then drive Reconcile once. The
	// deployed reconciler is at 0 replicas, so only our in-process pass moves
	// the Secret. With RotationOverlapWindow defaulting to 5m, the promote
	// happens on the SECOND Reconcile (the first call lands in PhaseStaged
	// and returns immediately after checkPropagation, leaving the staged
	// state visible on the apiserver).
	//
	// IMPORTANT: stageCertificate inside Reconcile re-reads the Secret AFTER
	// promotion-or-stage to decide which phase branch to dispatch. With an
	// empty Gate the staged branch promotes in the same pass.
	// So we want to inspect the Secret BEFORE the next Reconcile call, not
	// after — which means: trigger, Reconcile once (yields staged on disk
	// transiently and then promoted, all in one pass). To actually observe
	// the staged state on the apiserver we drive stage and promote in two
	// passes by toggling the gate. Simpler approach: assert staged shape by
	// reading the Secret right after stageCertificate runs, which happens at
	// the top of Reconcile, by using a Reconcile that promotes-but-leaves-
	// the-bundle. We accept that on disk the Secret transitions to active
	// in the SAME Reconcile pass when the Gate is empty.
	//
	// To observe staged distinctly we set a single non-existent gate CRD
	// for the first pass: checkPropagation returns false (the gate CRD is
	// missing on the target cluster), so handleStagedPhase requeues without
	// promoting. We then clear the gate and Reconcile again to promote.
	logWithTimestamp(t, "Step (a): Triggering force-rotation and driving Reconcile to enter staged phase...")
	if err := triggerRotation(ctx, t, hostingClient); err != nil {
		t.Fatalf("triggerRotation for staged phase: %v", err)
	}

	// Insert a sentinel gate that doesn't exist on the target cluster so the
	// first Reconcile lands in staged and requeues without promoting.
	webhookRec.Gate.Publish("test", []string{"does-not-exist-on-target.example.com"})
	if _, err := webhookRec.Reconcile(ctx, webhookReq); err != nil {
		t.Fatalf("Reconcile (stage phase): %v", err)
	}

	// (b) Inspect Secret in staged shape.
	logWithTimestamp(t, "Step (b): Asserting staged shape (2 certs, tls.key unchanged, tls.staged.key present)...")
	stagedSecret := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: controllerCertNs, Name: certSecretName}, stagedSecret); err != nil {
		t.Fatalf("Get cert Secret after stage: %v", err)
	}
	if got := stagedSecret.Annotations[controllers.RotationPhaseAnnotation]; got != "staged" {
		t.Errorf("staged phase annotation: want %q, got %q", "staged", got)
	}
	if stagedSecret.Annotations[controllers.RotationStartedAtAnnotation] == "" {
		t.Errorf("staged-started-at annotation must be set")
	}
	stagedChain, err := certificates.ParseChain(stagedSecret.Data["tls.crt"])
	if err != nil {
		t.Fatalf("ParseChain on staged tls.crt: %v", err)
	}
	if len(stagedChain) != 2 {
		t.Errorf("staged tls.crt: want 2 CERTIFICATE blocks, got %d", len(stagedChain))
	}
	if len(stagedSecret.Data["tls.staged.key"]) == 0 {
		t.Errorf("staged: tls.staged.key must be present and non-empty")
	}
	if !bytes.Equal(stagedSecret.Data["tls.key"], preStageKey) {
		t.Errorf("staged: tls.key must be UNCHANGED from the pre-stage leaf key")
	}

	// (c) Drive Reconcile again to advance staged → active. Clear the
	// sentinel gate first so propagation returns true and promote runs.
	logWithTimestamp(t, "Step (c): Clearing gate and driving Reconcile to promote staged → active...")
	webhookRec.Gate.Forget("test")
	if _, err := webhookRec.Reconcile(ctx, webhookReq); err != nil {
		t.Fatalf("Reconcile (promote phase): %v", err)
	}

	// (d) Inspect Secret in active shape.
	logWithTimestamp(t, "Step (d): Asserting active shape (still 2 certs, tls.key swapped, tls.staged.key gone)...")
	activeSecret := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: controllerCertNs, Name: certSecretName}, activeSecret); err != nil {
		t.Fatalf("Get cert Secret after promote: %v", err)
	}
	if got := activeSecret.Annotations[controllers.RotationPhaseAnnotation]; got != "active" {
		t.Errorf("active phase annotation: want %q, got %q", "active", got)
	}
	activeChain, err := certificates.ParseChain(activeSecret.Data["tls.crt"])
	if err != nil {
		t.Fatalf("ParseChain on active tls.crt: %v", err)
	}
	if len(activeChain) != 2 {
		t.Errorf("active tls.crt: want 2 CERTIFICATE blocks (overlap intact), got %d", len(activeChain))
	}
	if bytes.Equal(activeSecret.Data["tls.key"], preStageKey) {
		t.Errorf("active: tls.key must have CHANGED to the staged key (but matches pre-stage)")
	}
	if !bytes.Equal(activeSecret.Data["tls.key"], stagedSecret.Data["tls.staged.key"]) {
		t.Errorf("active: tls.key must equal the previously-staged key")
	}
	if _, present := activeSecret.Data["tls.staged.key"]; present {
		t.Errorf("active: tls.staged.key must be removed")
	}

	// (e) Shrink overlap window so the prune fires within budget, wait it
	// out, and Reconcile to prune.
	logWithTimestamp(t, "Step (e): Shrinking overlap window to 100ms, waiting 200ms, driving Reconcile to prune...")
	webhookRec.RotationOverlapWindow = 100 * time.Millisecond
	time.Sleep(200 * time.Millisecond)
	if _, err := webhookRec.Reconcile(ctx, webhookReq); err != nil {
		t.Fatalf("Reconcile (prune phase): %v", err)
	}

	prunedSecret := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: controllerCertNs, Name: certSecretName}, prunedSecret); err != nil {
		t.Fatalf("Get cert Secret after prune: %v", err)
	}
	prunedChain, err := certificates.ParseChain(prunedSecret.Data["tls.crt"])
	if err != nil {
		t.Fatalf("ParseChain on pruned tls.crt: %v", err)
	}
	if len(prunedChain) != 1 {
		t.Errorf("pruned tls.crt: want 1 CERTIFICATE block, got %d", len(prunedChain))
	}
	if got := prunedSecret.Annotations[controllers.RotationPhaseAnnotation]; got != "" {
		t.Errorf("pruned: phase annotation must be cleared, got %q", got)
	}
	if _, present := prunedSecret.Data["tls.staged.key"]; present {
		t.Errorf("pruned: tls.staged.key must be absent")
	}

	// Re-stamp the MR Secret via the MR reconciler and confirm the embedded
	// CRD's caBundle equals the single-cert (leaf-only) tls.crt.
	logWithTimestamp(t, "Re-stamping MR Secret with single-cert caBundle after prune...")
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("MR Reconcile after prune: %v", err)
	}
	gotSecretPostPrune := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, gotSecretPostPrune); err != nil {
		t.Fatalf("Get MR Secret after prune-restamp: %v", err)
	}
	postPruneCRD := extractCRDFromSecret(t, gotSecretPostPrune.Data["objects.yaml"], crdName)
	if !bytes.Equal(postPruneCRD.Spec.Conversion.Webhook.ClientConfig.CABundle, prunedSecret.Data["tls.crt"]) {
		t.Errorf("After prune: embedded CRD caBundle did not refresh to single-cert tls.crt\n got=%q\nwant=%q",
			string(postPruneCRD.Spec.Conversion.Webhook.ClientConfig.CABundle),
			string(prunedSecret.Data["tls.crt"]))
	}
	// Refresh newCACert to the post-prune single-cert bundle so the brotli
	// block below stamps against the current trust anchor rather than the
	// stale 2-cert overlap.
	newCACert = append([]byte(nil), prunedSecret.Data["tls.crt"]...)

	// --- 8. Brotli .br key codec exercise.
	// Replace the Secret's value with a brotli-encoded key. The reconciler
	// must transparently decompress, re-stamp (caBundle is already current
	// from step 7, so we mutate the embedded CRD back to a stale caBundle
	// to force a stamp), recompress, and write back.
	logWithTimestamp(t, "Exercising brotli .br codec...")

	// Build a fresh service-form CRD doc with NO caBundle so Reconcile is
	// guaranteed to write — otherwise the no-op fast-path masks the codec.
	freshCRD := serviceFormCRD(t, crdName, ns, "managedresourcewidget-conversion", convertPath)
	brData, err := managedresource.Encode(freshCRD, true)
	if err != nil {
		t.Fatalf("brotli Encode test fixture: %v", err)
	}
	// Re-fetch the MR Secret to capture the current resourceVersion. The
	// staged-rotation block above (step 7b) re-stamps the MR Secret as
	// part of the post-prune verification, so gotSecret3 is stale by the
	// time we reach this Update.
	freshSecret := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, freshSecret); err != nil {
		t.Fatalf("Re-fetch MR Secret before brotli patch: %v", err)
	}
	patchSecret := freshSecret.DeepCopy()
	patchSecret.Data = map[string][]byte{"objects.yaml.br": brData}
	if err := crClient.Update(ctx, patchSecret); err != nil {
		t.Fatalf("Update MR Secret with brotli payload: %v", err)
	}

	baselineBrotliKeys := readCounterVecValue(t, "webhook_injector_managed_resource_secret_keys_total", map[string]string{"encoding": "brotli"})
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile with brotli payload: %v", err)
	}
	assertDelta(t, "managed_resource_secret_keys_total{encoding=brotli}",
		baselineBrotliKeys,
		readCounterVecValue(t, "webhook_injector_managed_resource_secret_keys_total", map[string]string{"encoding": "brotli"}),
		1)

	gotSecret4 := &corev1.Secret{}
	if err := crClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretName}, gotSecret4); err != nil {
		t.Fatalf("Get MR Secret after brotli reconcile: %v", err)
	}
	brOut, ok := gotSecret4.Data["objects.yaml.br"]
	if !ok {
		t.Fatalf("Expected key objects.yaml.br to still exist after reconcile; keys: %v", mapKeys(gotSecret4.Data))
	}
	plain, _, err := managedresource.Decode(true, brOut)
	if err != nil {
		t.Fatalf("brotli Decode round-trip: %v", err)
	}
	brCRD := extractCRDFromSecret(t, plain, crdName)
	assertCRDStamped(t, brCRD, newCACert, expectedURL)

	logWithTimestamp(t, "TestManagedResourceFlow passed")
}

// --- helpers -----------------------------------------------------------------

const managedResourceCRDName = "managedresources.resources.gardener.cloud"

// kindClustersExist reports whether both `hosting` and `target` kind clusters
// are currently present. Used to decide between full setup and reuse when
// this test runs in a package with other e2e tests — running ./setup.sh per
// test exhausts CI runner Docker resources and produces flaky HTTP/2 hangs
// in unrelated tests that come later in the random `go test -shuffle` order.
func kindClustersExist(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("kind", "get", "clusters").Output()
	if err != nil {
		// `kind` not installed, or no clusters at all (kind exits 0 with empty
		// output when there are none; exits non-zero only on real errors).
		// Either way, conservative answer is "treat as absent" — the caller
		// will do a full setup, and if `kind` is missing setup.sh will fail
		// fast with a clear error.
		return false
	}
	names := strings.Split(strings.TrimSpace(string(out)), "\n")
	hostingFound, targetFound := false, false
	for _, n := range names {
		switch strings.TrimSpace(n) {
		case "hosting":
			hostingFound = true
		case "target":
			targetFound = true
		}
	}
	return hostingFound && targetFound
}

// installManagedResourceCRD applies a minimal Gardener ManagedResource CRD to
// the hosting cluster. We embed the schema rather than fetching from upstream
// to avoid network dependency. Only the fields the reconciler touches are
// schema-strict; everything else is x-kubernetes-preserve-unknown-fields.
func installManagedResourceCRD(t *testing.T, ctx context.Context, c ctrlclient.Client) {
	t.Helper()
	preserve := true
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: managedResourceCRDName},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "resources.gardener.cloud",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "ManagedResource",
				ListKind: "ManagedResourceList",
				Plural:   "managedresources",
				Singular: "managedresource",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:                   "object",
						XPreserveUnknownFields: &preserve,
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type:                   "object",
								XPreserveUnknownFields: &preserve,
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"secretRefs": {
										Type: "array",
										Items: &apiextensionsv1.JSONSchemaPropsOrArray{
											Schema: &apiextensionsv1.JSONSchemaProps{
												Type: "object",
												Properties: map[string]apiextensionsv1.JSONSchemaProps{
													"name": {Type: "string"},
												},
											},
										},
									},
									"keepObjects": {Type: "boolean"},
								},
							},
							"status": {
								Type:                   "object",
								XPreserveUnknownFields: &preserve,
							},
						},
					},
				},
			}},
		},
	}

	// Apply with create-or-update semantics: a previous run may have left
	// the CRD installed.
	existing := &apiextensionsv1.CustomResourceDefinition{}
	err := c.Get(ctx, types.NamespacedName{Name: managedResourceCRDName}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := c.Create(ctx, crd); err != nil {
			t.Fatalf("Create ManagedResource CRD: %v", err)
		}
	case err != nil:
		t.Fatalf("Get ManagedResource CRD: %v", err)
	default:
		// Already there — fine. Schema differences don't matter for our
		// test as long as the apiserver accepts the typed objects.
	}

	// Wait for the CRD to be Established so the apiserver can serve the resource.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		got := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, types.NamespacedName{Name: managedResourceCRDName}, got); err == nil {
			for _, cond := range got.Status.Conditions {
				if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("ManagedResource CRD did not reach Established status within 30s")
}

// serviceFormCRD returns a YAML document for a CRD whose conversion strategy
// is Webhook and whose clientConfig is Service-form (caBundle absent).
// Mirrors crdConversionYAMLTemplate but with explicit fields the reconciler
// will mutate.
func serviceFormCRD(t *testing.T, name, svcNs, svcName, svcPath string) []byte {
	t.Helper()
	svcPathCopy := svcPath
	crd := &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "ManagedResourceWidget",
				ListKind: "ManagedResourceWidgetList",
				Plural:   "managedresourcewidgets",
				Singular: "managedresourcewidget",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ConversionReviewVersions: []string{"v1"},
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						Service: &apiextensionsv1.ServiceReference{
							Namespace: svcNs,
							Name:      svcName,
							Path:      &svcPathCopy,
						},
					},
				},
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
					},
				},
				{
					Name:    "v2",
					Served:  true,
					Storage: false,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
					},
				},
			},
		},
	}
	out, err := sigsyaml.Marshal(crd)
	if err != nil {
		t.Fatalf("Marshal CRD fixture: %v", err)
	}
	return out
}

// extractCRDFromSecret finds the CRD doc with the given name inside a multi-
// doc YAML payload and decodes it as a CRD. Fails the test if no such doc
// is present. crdName parameterised for future callers; this test only
// uses one CRD name today.
func extractCRDFromSecret(t *testing.T, payload []byte, crdName string) *apiextensionsv1.CustomResourceDefinition { //nolint:unparam // single CRD today, kept parametric for symmetry with extractCRD callers
	t.Helper()
	docs, err := managedresource.SplitDocs(payload)
	if err != nil {
		t.Fatalf("SplitDocs: %v", err)
	}
	for _, d := range docs {
		var typeOnly metav1.TypeMeta
		if err := sigsyaml.Unmarshal(d, &typeOnly); err != nil {
			continue
		}
		if typeOnly.Kind != "CustomResourceDefinition" {
			continue
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := sigsyaml.Unmarshal(d, crd); err != nil {
			t.Fatalf("Unmarshal CRD doc: %v", err)
		}
		if crd.Name == crdName {
			return crd
		}
	}
	t.Fatalf("CRD %q not found in payload: %s", crdName, string(payload))
	return nil
}

// assertCRDStamped checks that the CRD carries the expected caBundle, that
// clientConfig.URL has been written to expectedURL, and that Service was
// cleared.
func assertCRDStamped(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, wantCABundle []byte, wantURL string) {
	t.Helper()
	if crd.Spec.Conversion == nil || crd.Spec.Conversion.Webhook == nil || crd.Spec.Conversion.Webhook.ClientConfig == nil {
		t.Fatalf("CRD missing conversion.webhook.clientConfig: %+v", crd.Spec.Conversion)
	}
	cc := crd.Spec.Conversion.Webhook.ClientConfig
	if !bytes.Equal(cc.CABundle, wantCABundle) {
		t.Errorf("caBundle mismatch:\n got=%q\nwant=%q", string(cc.CABundle), string(wantCABundle))
	}
	if cc.URL == nil || *cc.URL != wantURL {
		t.Errorf("URL mismatch: got=%v want=%s", cc.URL, wantURL)
	}
	if cc.Service != nil {
		t.Errorf("Service was not cleared after URL rewrite: %+v", cc.Service)
	}
}

// getCABundle reads tls.crt from the controller's cert Secret. The Secret
// must exist — caller must have waited for the WebhookReconciler to populate
// it (we wait for the webhook-server Deployment above to handle that).
func getCABundle(t *testing.T, ctx context.Context, c ctrlclient.Client, ns, name string) []byte {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s); err != nil {
		t.Fatalf("Get cert Secret %s/%s: %v", ns, name, err)
	}
	ca := s.Data["tls.crt"]
	if len(ca) == 0 {
		t.Fatalf("cert Secret %s/%s has empty tls.crt", ns, name)
	}
	return ca
}

// waitForCABundleChange polls the cert Secret until tls.crt differs from the
// given baseline, or the deadline fires.
func waitForCABundleChange(t *testing.T, ctx context.Context, c ctrlclient.Client, ns, name string, old []byte, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s); err == nil {
			cur := s.Data["tls.crt"]
			if len(cur) > 0 && !bytes.Equal(cur, old) {
				return cur
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("Cert Secret tls.crt did not change within timeout after forced rotation")
	return nil
}

func cleanupTestObjects(t *testing.T, ctx context.Context, c ctrlclient.Client, ns, mrName, secretName string) {
	t.Helper()
	// Best-effort deletion — IsNotFound is fine.
	_ = c.Delete(ctx, &resourcesv1alpha1.ManagedResource{ObjectMeta: metav1.ObjectMeta{Name: mrName, Namespace: ns}}) //nolint:errcheck
	_ = c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns}})                 //nolint:errcheck
}

// readCounterValue reads a non-vec counter from the controller-runtime metrics
// registry. Returns 0 if the metric is not present.
func readCounterValue(t *testing.T, name string) float64 {
	t.Helper()
	return readMetric(t, name, nil)
}

// readCounterVecValue reads a labeled counter from the controller-runtime
// metrics registry. Returns 0 if no metric matches.
func readCounterVecValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	return readMetric(t, name, labels)
}

func readMetric(t *testing.T, name string, wantLabels map[string]string) float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather metrics: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if !metricLabelsMatch(m.GetLabel(), wantLabels) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
		}
	}
	return 0
}

func metricLabelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	matched := 0
	for _, lp := range got {
		if v, ok := want[lp.GetName()]; ok && v == lp.GetValue() {
			matched++
		}
	}
	return matched == len(want)
}

func assertDelta(t *testing.T, label string, before, after, wantAtLeast float64) { //nolint:unparam // wantAtLeast is a knob — every caller currently wants >=1 but the test will grow.
	t.Helper()
	delta := after - before
	if delta < wantAtLeast {
		t.Errorf("Metric %s: expected delta >= %.0f, got %.0f (before=%.0f after=%.0f)",
			label, wantAtLeast, delta, before, after)
	}
}

func mapKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// scaleDeployment sets the deployment's spec.replicas via the Scale subresource
// and returns the prior replica count so the caller can restore it on cleanup.
// We use the Scale subresource (not Update on the full Deployment) to avoid
// racing the deployment controller on resourceVersion of fields we don't care
// about.
func scaleDeployment(t *testing.T, ctx context.Context, client *kubernetes.Clientset, ns, name string, replicas int32) int32 {
	t.Helper()
	scale, err := client.AppsV1().Deployments(ns).GetScale(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("GetScale %s/%s: %v", ns, name, err)
	}
	prev := scale.Spec.Replicas
	if prev == replicas {
		return prev
	}
	scale.Spec.Replicas = replicas
	if _, err := client.AppsV1().Deployments(ns).UpdateScale(ctx, name, scale, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("UpdateScale %s/%s -> %d: %v", ns, name, replicas, err)
	}
	return prev
}

// waitForDeploymentScale polls until the Deployment reports the desired number
// of ready replicas (matching observedGeneration). Used to ensure the deployed
// reconciler is quiesced before driving the phase machine in-process.
func waitForDeploymentScale(t *testing.T, client *kubernetes.Clientset, name string, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := client.AppsV1().Deployments("default").Get(context.Background(), name, metav1.GetOptions{})
		if err == nil && dep.Status.ObservedGeneration >= dep.Generation && dep.Status.Replicas == want && dep.Status.ReadyReplicas == want {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("Deployment %s did not reach %d replicas within %s", name, want, timeout)
}
