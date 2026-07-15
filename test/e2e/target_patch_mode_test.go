// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
)

// TestTargetPatchModeFlow exercises TargetWebhookReconciler end-to-end against
// the real kind-target apiserver. It:
//
//  1. Applies a labeled CRD (conversion.strategy=Webhook), a labeled
//     MutatingWebhookConfiguration, and a labeled ValidatingWebhookConfiguration
//     DIRECTLY to the target cluster — no ManagedResource, mirroring the
//     GRM-free dual-deployment-operator topology.
//  2. Drives ReconcileCRD/ReconcileMWC/ReconcileVWC in-process against the
//     target cluster and asserts each object's caBundle is stamped with the
//     hosting cert Secret's tls.crt.
//  3. Confirms the CRD is published on the rotation gate (MWC/VWC are not).
//  4. Simulates a rotation by changing the hosting cert Secret's tls.crt and
//     asserts a re-reconcile propagates the new caBundle.
//
// Gating mirrors TestManagedResourceFlow: reuse existing kind clusters when
// present; only do a full setup when none exist and -test.reuse-clusters is
// unset.
func TestTargetPatchModeFlow(t *testing.T) {
	kubeconfigPath, absErr := filepath.Abs(filepath.Join(".", "kubeconfig"))
	if absErr != nil {
		t.Fatalf("Resolve kubeconfig path: %v", absErr)
	}
	t.Setenv("KUBECONFIG", kubeconfigPath)

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

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

	// The hosting cert Secret's tls.crt is populated by the deployed
	// ConfigMap-mode WebhookReconciler. Wait for the deployment so we don't
	// race startup before reading the caBundle.
	logWithTimestamp(t, "Waiting for webhook deployment ready...")
	waitForDeploymentReady(t, hostingClient)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiextensionsv1.AddToScheme(scheme))
	utilruntime.Must(admissionv1.AddToScheme(scheme))

	hostingCfg := loadContextConfig(t, kubeconfigPath, "kind-hosting")
	hostingCRClient, err := ctrlclient.New(hostingCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Build hosting controller-runtime client: %v", err)
	}
	targetCfg := loadContextConfig(t, kubeconfigPath, "kind-target")
	targetCRClient, err := ctrlclient.New(targetCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Build target controller-runtime client: %v", err)
	}

	const (
		ns             = "default"
		certSecretName = "webhook-injector-certs"
		labelKey       = "webhook-injector.sap.com/test-patch"
		labelVal       = "true"
		crdName        = "widgets.patch.example.com"
		mwcName        = "patch-e2e-mutating"
		vwcName        = "patch-e2e-validating"
	)

	// --- 1. Apply labeled CRD/MWC/VWC DIRECTLY to the target cluster.
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: crdName, Labels: map[string]string{labelKey: labelVal}},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "patch.example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:     "Widget",
				ListKind: "WidgetList",
				Plural:   "widgets",
				Singular: "widget",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1",
					Served:  true,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: new(true)},
					},
				},
				{
					Name:    "v2",
					Served:  true,
					Storage: false,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: new(true)},
					},
				},
			},
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ConversionReviewVersions: []string{"v1"},
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						URL: new("https://webhook.example.com:9443/convert"),
					},
				},
			},
		},
	}
	if err := targetCRClient.Create(ctx, crd); err != nil {
		t.Fatalf("Create target CRD: %v", err)
	}
	t.Cleanup(func() {
		_ = targetCRClient.Delete(context.Background(), &apiextensionsv1.CustomResourceDefinition{ //nolint:errcheck
			ObjectMeta: metav1.ObjectMeta{Name: crdName},
		})
	})

	sideEffects := admissionv1.SideEffectClassNone
	mwc := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: mwcName, Labels: map[string]string{labelKey: labelVal}},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name:                    "m.patch.example.com",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			ClientConfig:            admissionv1.WebhookClientConfig{URL: new("https://webhook.example.com:9443/mutate")},
		}},
	}
	if err := targetCRClient.Create(ctx, mwc); err != nil {
		t.Fatalf("Create target MWC: %v", err)
	}
	t.Cleanup(func() {
		_ = targetCRClient.Delete(context.Background(), &admissionv1.MutatingWebhookConfiguration{ //nolint:errcheck
			ObjectMeta: metav1.ObjectMeta{Name: mwcName},
		})
	})

	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: vwcName, Labels: map[string]string{labelKey: labelVal}},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:                    "v.patch.example.com",
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &sideEffects,
			ClientConfig:            admissionv1.WebhookClientConfig{URL: new("https://webhook.example.com:9443/validate")},
		}},
	}
	if err := targetCRClient.Create(ctx, vwc); err != nil {
		t.Fatalf("Create target VWC: %v", err)
	}
	t.Cleanup(func() {
		_ = targetCRClient.Delete(context.Background(), &admissionv1.ValidatingWebhookConfiguration{ //nolint:errcheck
			ObjectMeta: metav1.ObjectMeta{Name: vwcName},
		})
	})

	// --- 2. In-process TargetWebhookReconciler against the target cluster.
	// CertSecretReader reads the hosting cert Secret (populated by the
	// deployed ConfigMap-mode reconciler).
	gate := rotationgate.New()
	rec := &controllers.TargetWebhookReconciler{
		TargetClient:     targetCRClient,
		CertSecretReader: hostingCRClient,
		Namespace:        ns,
		CertSecretName:   certSecretName,
		LabelKey:         labelKey,
		LabelValue:       labelVal,
		Gate:             gate,
	}

	caBundle := getCABundle(t, ctx, hostingCRClient, ns, certSecretName)

	reconcileAll := func() {
		if _, err := rec.ReconcileCRD(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: crdName}}); err != nil {
			t.Fatalf("ReconcileCRD: %v", err)
		}
		if _, err := rec.ReconcileMWC(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: mwcName}}); err != nil {
			t.Fatalf("ReconcileMWC: %v", err)
		}
		if _, err := rec.ReconcileVWC(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: vwcName}}); err != nil {
			t.Fatalf("ReconcileVWC: %v", err)
		}
	}
	reconcileAll()

	assertCRDCABundle(t, ctx, targetCRClient, crdName, caBundle)
	assertMWCCABundle(t, ctx, targetCRClient, mwcName, caBundle)
	assertVWCCABundle(t, ctx, targetCRClient, vwcName, caBundle)

	// --- 3. CRD is on the rotation gate; MWC/VWC are not.
	snap := gate.Snapshot()
	if len(snap) != 1 || snap[0] != crdName {
		t.Errorf("gate snapshot = %v, want [%s] (only CRDs are gated)", snap, crdName)
	}

	// --- 4. Simulate rotation: change the hosting cert Secret tls.crt, then
	// re-reconcile and assert the new caBundle propagates to all three.
	newCA := append([]byte("rotated-"), caBundle...)
	patchCertSecretTLSCrt(t, ctx, hostingCRClient, ns, certSecretName, newCA)
	reconcileAll()
	assertCRDCABundle(t, ctx, targetCRClient, crdName, newCA)
	assertMWCCABundle(t, ctx, targetCRClient, mwcName, newCA)
	assertVWCCABundle(t, ctx, targetCRClient, vwcName, newCA)

	logWithTimestamp(t, "Target patch-mode flow verified: CRD/MWC/VWC caBundle stamped and rotated")
}

func assertCRDCABundle(t *testing.T, ctx context.Context, c ctrlclient.Client, name string, want []byte) {
	t.Helper()
	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("Get target CRD %s: %v", name, err)
	}
	cc := got.Spec.Conversion.Webhook.ClientConfig
	if !bytes.Equal(cc.CABundle, want) {
		t.Errorf("CRD %s caBundle mismatch: got %q want %q", name, cc.CABundle, want)
	}
}

func assertMWCCABundle(t *testing.T, ctx context.Context, c ctrlclient.Client, name string, want []byte) {
	t.Helper()
	got := &admissionv1.MutatingWebhookConfiguration{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("Get target MWC %s: %v", name, err)
	}
	for i, w := range got.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, want) {
			t.Errorf("MWC %s webhook[%d] caBundle mismatch: got %q want %q", name, i, w.ClientConfig.CABundle, want)
		}
	}
}

func assertVWCCABundle(t *testing.T, ctx context.Context, c ctrlclient.Client, name string, want []byte) {
	t.Helper()
	got := &admissionv1.ValidatingWebhookConfiguration{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, got); err != nil {
		t.Fatalf("Get target VWC %s: %v", name, err)
	}
	for i, w := range got.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, want) {
			t.Errorf("VWC %s webhook[%d] caBundle mismatch: got %q want %q", name, i, w.ClientConfig.CABundle, want)
		}
	}
}

// patchCertSecretTLSCrt overwrites tls.crt on the hosting cert Secret to
// simulate a rotation for the in-process patch-mode reconcile assertions.
func patchCertSecretTLSCrt(t *testing.T, ctx context.Context, c ctrlclient.Client, ns, name string, tlsCrt []byte) {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, s); err != nil {
		t.Fatalf("Get cert Secret for patch: %v", err)
	}
	base := s.DeepCopy()
	s.Data["tls.crt"] = tlsCrt
	if err := c.Patch(ctx, s, ctrlclient.MergeFrom(base)); err != nil {
		t.Fatalf("Patch cert Secret tls.crt: %v", err)
	}
}
