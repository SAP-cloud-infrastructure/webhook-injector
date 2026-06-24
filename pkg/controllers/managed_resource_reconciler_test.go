// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"bytes"
	"context"
	"testing"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sigsyaml "sigs.k8s.io/yaml"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/managedresource"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/metrics"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/webhookhost"
)

// testScheme assembles a scheme with the core types, apiextensions (CRD) and
// the gardener resources v1alpha1 API. The reconciler under test reads
// ManagedResource and Secret objects, and decodes embedded CRD documents.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatalf("apiextensionsv1.AddToScheme: %v", err)
	}
	if err := resourcesv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("resourcesv1alpha1.AddToScheme: %v", err)
	}
	return s
}

// countingClient wraps a client.Client and counts Update / Patch calls. Used
// to assert the "no-op reconcile produces zero Secret Updates" idempotency
// contract, and to verify the force-reconcile Patch is only issued when at
// least one Secret was actually written.
type countingClient struct {
	client.Client
	updates   int
	mrPatches int // count of Patch calls against a ManagedResource
}

func (c *countingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates++
	return c.Client.Update(ctx, obj, opts...)
}

func (c *countingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if _, ok := obj.(*resourcesv1alpha1.ManagedResource); ok {
		c.mrPatches++
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// Test fixture constants.
const (
	testCABundle      = "test-ca-bundle-bytes"
	mrName            = "test-mr"
	mrNamespace       = "garden-shoot"
	secretName        = "test-mr-resources"
	certSecretName    = "webhook-injector-certs"
	webhookLabelKey   = "webhook-injector/name"
	webhookLabelValue = "shoot-cert-manager"
	externalHost      = "webhooks.example.com:9443"
)

// crdDocServiceForm returns a CRD with conversion.strategy=Webhook and a
// Service-form clientConfig, with no caBundle/url stamped. The fixed
// Service.Path "/convert" exercises the URL-rewrite composition.
func crdDocServiceForm(t *testing.T) []byte {
	t.Helper()
	const (
		name = "widgets.example.com"
		path = "/convert"
	)
	p := path
	crd := &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       crdKind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "widgets",
				Singular: "widget",
				Kind:     "Widget",
				ListKind: "WidgetList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true, Storage: true, Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
				}},
			},
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ConversionReviewVersions: []string{"v1"},
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						Service: &apiextensionsv1.ServiceReference{
							Namespace: "ns",
							Name:      "svc",
							Path:      &p,
						},
					},
				},
			},
		},
	}
	out, err := sigsyaml.Marshal(crd)
	if err != nil {
		t.Fatalf("marshal CRD %s: %v", name, err)
	}
	return out
}

// crdDocStrategyNone returns a CRD with conversion.strategy=None — should be
// untouched by the reconciler.
func crdDocStrategyNone(t *testing.T, name string) []byte {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       crdKind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "gadgets", Singular: "gadget", Kind: "Gadget", ListKind: "GadgetList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true, Storage: true, Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
				}},
			},
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.NoneConverter,
			},
		},
	}
	out, err := sigsyaml.Marshal(crd)
	if err != nil {
		t.Fatalf("marshal CRD %s: %v", name, err)
	}
	return out
}

// nonCRDDoc returns a plain YAML doc that is decidedly not a CRD.
func nonCRDDoc(name string) []byte {
	return []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\ndata:\n  hello: world\n")
}

// crdDocStamped returns a CRD already converged with the given caBundle and
// URL — used to test the no-op idempotency path.
func crdDocStamped(t *testing.T, name, url string, caBundle []byte) []byte {
	t.Helper()
	u := url
	crd := &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       crdKind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "widgets", Singular: "widget", Kind: "Widget", ListKind: "WidgetList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true, Storage: true, Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"},
				}},
			},
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ConversionReviewVersions: []string{"v1"},
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						URL:      &u,
						CABundle: caBundle,
					},
				},
			},
		},
	}
	out, err := sigsyaml.Marshal(crd)
	if err != nil {
		t.Fatalf("marshal stamped CRD %s: %v", name, err)
	}
	return out
}

// makeCertSecret produces the hosting-cluster certificate Secret tracked by
// the reconciler's CertSecretName.
func makeCertSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      certSecretName,
			Namespace: mrNamespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			tlsCrtKey: []byte(testCABundle),
			"tls.key": []byte("test-key"),
		},
	}
}

// makeMR returns a labeled ManagedResource referencing a single Secret named
// secretName in the same namespace. spec.keepObjects is set to true so the
// reconciler does not log a keepObjects-violation against the test MR; the
// dedicated KeepObjectsViolation* subtests opt out of this default.
func makeMR(labels map[string]string) *resourcesv1alpha1.ManagedResource {
	t := true
	return &resourcesv1alpha1.ManagedResource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mrName,
			Namespace: mrNamespace,
			Labels:    labels,
		},
		Spec: resourcesv1alpha1.ManagedResourceSpec{
			SecretRefs:  []corev1.LocalObjectReference{{Name: secretName}},
			KeepObjects: &t,
		},
	}
}

// makeResourceSecret builds the gardener-resource-manager-style resource
// Secret with the given data keys / values. Values are taken verbatim — the
// caller decides whether they are brotli-compressed.
func makeResourceSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: mrNamespace,
		},
		Data: data,
	}
}

// reconcileOnce builds a reconciler around the given counting client and
// invokes Reconcile for the canonical MR/Namespace pair.
func reconcileOnce(t *testing.T, cc *countingClient) {
	t.Helper()
	reconcileOnceWith(t, cc, true)
}

// reconcileOnceWith allows tests to opt out of the force-reconcile annotation
// path. Default callers (reconcileOnce) get the production default (true).
func reconcileOnceWith(t *testing.T, cc *countingClient, forceReconcile bool) {
	t.Helper()
	r := &ManagedResourceReconciler{
		HostingClient:     cc,
		Namespace:         mrNamespace,
		CertSecretName:    certSecretName,
		WebhookLabelKey:   webhookLabelKey,
		WebhookLabelValue: webhookLabelValue,
		ExternalHost:      externalHost,
		ForceReconcile:    forceReconcile,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: mrName, Namespace: mrNamespace},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// extractCRDFromSecret pulls the (possibly compressed) document at the given
// key from the resource Secret, splits the multi-doc YAML, returns docs and
// whether the key was brotli-suffixed.
func extractDocsFromSecret(t *testing.T, c client.Client, key string) ([][]byte, bool) {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: secretName, Namespace: mrNamespace}, secret); err != nil {
		t.Fatalf("get resource secret: %v", err)
	}
	raw, ok := secret.Data[key]
	if !ok {
		t.Fatalf("resource secret missing key %q; keys=%v", key, secretKeys(secret.Data))
	}
	plain, compressed, err := managedresource.Decode(managedresource.IsCompressedKey(key), raw)
	if err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	docs, err := managedresource.SplitDocs(plain)
	if err != nil {
		t.Fatalf("split %s: %v", key, err)
	}
	return docs, compressed
}

func secretKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// findCRD locates the first apiextensions.k8s.io/v1 CRD doc and unmarshals
// it.
func findCRD(t *testing.T, docs [][]byte) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	for i, d := range docs {
		var tm metav1.TypeMeta
		if err := sigsyaml.Unmarshal(d, &tm); err != nil {
			continue
		}
		if tm.APIVersion != "apiextensions.k8s.io/v1" || tm.Kind != crdKind {
			continue
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := sigsyaml.Unmarshal(d, crd); err != nil {
			t.Fatalf("unmarshal CRD doc %d: %v", i, err)
		}
		return crd
	}
	t.Fatalf("no CRD doc found among %d docs", len(docs))
	return nil
}

func TestManagedResourceReconciler(t *testing.T) {
	t.Run("PlainMultiDocStampsCRDAndPreservesOthers", func(t *testing.T) {
		s := testScheme(t)

		cm1 := nonCRDDoc("cm-before")
		crdSvc := crdDocServiceForm(t)
		cm2 := nonCRDDoc("cm-after")
		plain := managedresource.JoinDocs([][]byte{cm1, crdSvc, cm2})

		encoded, err := managedresource.Encode(plain, false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		resourceSecret := makeResourceSecret(map[string][]byte{"objects.yaml": encoded})

		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, resourceSecret, makeCertSecret()).
			Build()
		cc := &countingClient{Client: base}

		reconcileOnce(t, cc)

		if cc.updates != 1 {
			t.Fatalf("expected exactly 1 Secret Update, got %d", cc.updates)
		}
		if cc.mrPatches != 1 {
			t.Fatalf("expected exactly 1 ManagedResource Patch (force-reconcile annotation), got %d", cc.mrPatches)
		}

		docs, compressed := extractDocsFromSecret(t, cc, "objects.yaml")
		if compressed {
			t.Fatalf("expected uncompressed key, got compressed payload")
		}
		if len(docs) != 3 {
			t.Fatalf("expected 3 docs after reconcile, got %d", len(docs))
		}

		// Doc 0 and doc 2 (the ConfigMaps) should be byte-identical to the
		// originals: untouched docs are kept verbatim (no YAML round-trip).
		// We compare against the SplitDocs output of the original payload so
		// trailing newlines line up.
		origDocs, err := managedresource.SplitDocs(plain)
		if err != nil {
			t.Fatalf("split original: %v", err)
		}
		if !bytes.Equal(docs[0], origDocs[0]) {
			t.Errorf("ConfigMap doc 0 mutated:\n got=%q\nwant=%q", docs[0], origDocs[0])
		}
		if !bytes.Equal(docs[2], origDocs[2]) {
			t.Errorf("ConfigMap doc 2 mutated:\n got=%q\nwant=%q", docs[2], origDocs[2])
		}

		crd := findCRD(t, docs)
		cc2 := crd.Spec.Conversion.Webhook.ClientConfig
		if !bytes.Equal(cc2.CABundle, []byte(testCABundle)) {
			t.Errorf("CRD caBundle: got %q, want %q", cc2.CABundle, testCABundle)
		}
		if cc2.Service != nil {
			t.Errorf("expected clientConfig.service to be cleared, got %+v", cc2.Service)
		}
		path := "/convert"
		wantURL := webhookhost.BuildExternalURL(externalHost, &path)
		if cc2.URL == nil || *cc2.URL != wantURL {
			t.Errorf("CRD clientConfig.url: got %v, want %q", cc2.URL, wantURL)
		}
	})

	t.Run("BrotliCompressedKey", func(t *testing.T) {
		s := testScheme(t)

		cm1 := nonCRDDoc("cm-before")
		crdSvc := crdDocServiceForm(t)
		plain := managedresource.JoinDocs([][]byte{cm1, crdSvc})

		encoded, err := managedresource.Encode(plain, true)
		if err != nil {
			t.Fatalf("brotli encode: %v", err)
		}
		resourceSecret := makeResourceSecret(map[string][]byte{"objects.yaml.br": encoded})

		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, resourceSecret, makeCertSecret()).
			Build()
		cc := &countingClient{Client: base}

		reconcileOnce(t, cc)

		if cc.updates != 1 {
			t.Fatalf("expected exactly 1 Secret Update, got %d", cc.updates)
		}
		if cc.mrPatches != 1 {
			t.Fatalf("expected exactly 1 ManagedResource Patch (force-reconcile annotation), got %d", cc.mrPatches)
		}

		// Key remains brotli-suffixed; decoding should still succeed.
		secret := &corev1.Secret{}
		if err := cc.Get(context.Background(), types.NamespacedName{Name: secretName, Namespace: mrNamespace}, secret); err != nil {
			t.Fatalf("get secret: %v", err)
		}
		if _, ok := secret.Data["objects.yaml.br"]; !ok {
			t.Fatalf("expected key objects.yaml.br to be preserved, keys=%v", secretKeys(secret.Data))
		}
		if _, ok := secret.Data["objects.yaml"]; ok {
			t.Fatalf("unexpected uncompressed key 'objects.yaml' present after reconcile")
		}

		docs, compressed := extractDocsFromSecret(t, cc, "objects.yaml.br")
		if !compressed {
			t.Fatalf("expected compressed key, got uncompressed payload")
		}
		if len(docs) != 2 {
			t.Fatalf("expected 2 docs, got %d", len(docs))
		}
		crd := findCRD(t, docs)
		cc2 := crd.Spec.Conversion.Webhook.ClientConfig
		if !bytes.Equal(cc2.CABundle, []byte(testCABundle)) {
			t.Errorf("CRD caBundle: got %q, want %q", cc2.CABundle, testCABundle)
		}
		path := "/convert"
		wantURL := webhookhost.BuildExternalURL(externalHost, &path)
		if cc2.URL == nil || *cc2.URL != wantURL {
			t.Errorf("CRD clientConfig.url: got %v, want %q", cc2.URL, wantURL)
		}
		if cc2.Service != nil {
			t.Errorf("expected clientConfig.service to be cleared")
		}
	})

	t.Run("StrategyNoneLeftAlone", func(t *testing.T) {
		s := testScheme(t)

		crdNone := crdDocStrategyNone(t, "gadgets.example.com")
		plain := managedresource.JoinDocs([][]byte{crdNone})

		encoded, err := managedresource.Encode(plain, false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		resourceSecret := makeResourceSecret(map[string][]byte{"objects.yaml": encoded})

		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, resourceSecret, makeCertSecret()).
			Build()
		cc := &countingClient{Client: base}

		reconcileOnce(t, cc)

		if cc.updates != 0 {
			t.Fatalf("expected 0 Secret Updates for strategy=None, got %d", cc.updates)
		}
		if cc.mrPatches != 0 {
			t.Fatalf("expected 0 ManagedResource Patches when no Secret was written, got %d", cc.mrPatches)
		}
	})

	t.Run("AlreadyStampedIsIdempotent", func(t *testing.T) {
		s := testScheme(t)

		path := "/convert"
		wantURL := webhookhost.BuildExternalURL(externalHost, &path)
		crdOK := crdDocStamped(t, "widgets.example.com", wantURL, []byte(testCABundle))
		plain := managedresource.JoinDocs([][]byte{crdOK})

		encoded, err := managedresource.Encode(plain, false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		resourceSecret := makeResourceSecret(map[string][]byte{"objects.yaml": encoded})

		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, resourceSecret, makeCertSecret()).
			Build()
		cc := &countingClient{Client: base}

		reconcileOnce(t, cc)

		if cc.updates != 0 {
			t.Fatalf("expected 0 Secret Updates on already-stamped CRD, got %d", cc.updates)
		}
		if cc.mrPatches != 0 {
			t.Fatalf("expected 0 ManagedResource Patches on idempotent no-op reconcile, got %d", cc.mrPatches)
		}
	})

	t.Run("NonCRDDocsLeftAlone", func(t *testing.T) {
		s := testScheme(t)

		// A Deployment-shaped YAML doc (not a CRD) — the GVK filter inside
		// maybeStampCRDDoc must bail out before attempting CRD unmarshal.
		deploymentDoc := []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: foo\nspec:\n  replicas: 1\n")
		svcDoc := []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: bar\nspec:\n  ports:\n    - port: 80\n")
		plain := managedresource.JoinDocs([][]byte{deploymentDoc, svcDoc})

		encoded, err := managedresource.Encode(plain, false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		resourceSecret := makeResourceSecret(map[string][]byte{"objects.yaml": encoded})

		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, resourceSecret, makeCertSecret()).
			Build()
		cc := &countingClient{Client: base}

		reconcileOnce(t, cc)

		if cc.updates != 0 {
			t.Fatalf("expected 0 Secret Updates when only non-CRD docs are present, got %d", cc.updates)
		}
		if cc.mrPatches != 0 {
			t.Fatalf("expected 0 ManagedResource Patches when no Secret was written, got %d", cc.mrPatches)
		}
	})

	t.Run("UnlabeledMRIsNoop", func(t *testing.T) {
		s := testScheme(t)

		// Even though the data shape would normally produce a stamp, the MR
		// lacks the webhook label so the in-Reconcile re-check short-circuits
		// before any Secret IO.
		crdSvc := crdDocServiceForm(t)
		plain := managedresource.JoinDocs([][]byte{crdSvc})

		encoded, err := managedresource.Encode(plain, false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		resourceSecret := makeResourceSecret(map[string][]byte{"objects.yaml": encoded})

		mr := makeMR(map[string]string{"some.other/label": "value"})

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, resourceSecret, makeCertSecret()).
			Build()
		cc := &countingClient{Client: base}

		reconcileOnce(t, cc)

		if cc.updates != 0 {
			t.Fatalf("expected 0 Secret Updates for unlabeled MR, got %d", cc.updates)
		}
		if cc.mrPatches != 0 {
			t.Fatalf("expected 0 ManagedResource Patches for unlabeled MR, got %d", cc.mrPatches)
		}
	})

	t.Run("DanglingSecretRefIsNoop", func(t *testing.T) {
		s := testScheme(t)

		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})

		// Deliberately don't add the resource Secret — only the cert Secret
		// and the ManagedResource exist. The reconciler must Get the
		// referenced Secret, see NotFound, log+skip, and return no error.
		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret()).
			Build()
		cc := &countingClient{Client: base}

		r := &ManagedResourceReconciler{
			HostingClient:     cc,
			Namespace:         mrNamespace,
			CertSecretName:    certSecretName,
			WebhookLabelKey:   webhookLabelKey,
			WebhookLabelValue: webhookLabelValue,
			ExternalHost:      externalHost,
			ForceReconcile:    true,
		}
		res, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: mrName, Namespace: mrNamespace},
		})
		if err != nil {
			t.Fatalf("Reconcile returned error for dangling secretRef: %v", err)
		}
		if res.RequeueAfter != 0 {
			t.Errorf("expected zero-value ctrl.Result, got %+v", res)
		}
		if cc.updates != 0 {
			t.Fatalf("expected 0 Secret Updates with dangling secretRef, got %d", cc.updates)
		}
		if cc.mrPatches != 0 {
			t.Fatalf("expected 0 ManagedResource Patches with dangling secretRef, got %d", cc.mrPatches)
		}
	})

	t.Run("ForceReconcileDisabled", func(t *testing.T) {
		// When ForceReconcile is false the reconciler must still write the
		// Secret on a real stamp, but must NOT patch the MR annotation —
		// users opting out trust GRM's Secret watch entirely.
		s := testScheme(t)
		docs := [][]byte{crdDocServiceForm(t)}
		raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
			Data:       map[string][]byte{"objects.yaml": raw},
		}
		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret(), secret).
			Build()
		cc := &countingClient{Client: base}

		reconcileOnceWith(t, cc, false)

		if cc.updates != 1 {
			t.Fatalf("expected exactly 1 Secret Update, got %d", cc.updates)
		}
		if cc.mrPatches != 0 {
			t.Fatalf("expected 0 ManagedResource Patches with ForceReconcile=false, got %d", cc.mrPatches)
		}

		// And the MR must not carry the operation annotation.
		got := &resourcesv1alpha1.ManagedResource{}
		if err := cc.Get(context.Background(), types.NamespacedName{Name: mrName, Namespace: mrNamespace}, got); err != nil {
			t.Fatalf("get MR: %v", err)
		}
		if v := got.Annotations[gardenerOperationAnnotation]; v != "" {
			t.Fatalf("expected MR annotation %s to be absent, got %q", gardenerOperationAnnotation, v)
		}
	})

	t.Run("ForceReconcileSkipsWhenAnnotationAlreadyPresent", func(t *testing.T) {
		// A previous reconcile may have already stamped the annotation and
		// GRM has not yet consumed it. The reconciler must NOT re-Patch with
		// the same value — that would generate a no-op MR write per
		// reconcile until GRM gets around to consuming the annotation.
		s := testScheme(t)
		// Pre-stamp the CRD so the Secret-write path doesn't fire; the only
		// thing exercising the MR-patch branch would be a *change* of
		// annotation state, and there is no change here.
		path := "/convert"
		stampedURL := webhookhost.BuildExternalURL(externalHost, &path)
		docs := [][]byte{crdDocStamped(t, "widgets.example.com", stampedURL, []byte(testCABundle))}
		raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
			Data:       map[string][]byte{"objects.yaml": raw},
		}
		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})
		mr.Annotations = map[string]string{gardenerOperationAnnotation: gardenerOperationReconcile}

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret(), secret).
			Build()
		cc := &countingClient{Client: base}

		reconcileOnce(t, cc)

		// Already stamped + already annotated → zero writes of any kind.
		if cc.updates != 0 {
			t.Fatalf("expected 0 Secret Updates on full convergence, got %d", cc.updates)
		}
		if cc.mrPatches != 0 {
			t.Fatalf("expected 0 ManagedResource Patches when annotation already present, got %d", cc.mrPatches)
		}
	})

	t.Run("KeepObjectsViolationWhenUnsetAndCRDsPresent", func(t *testing.T) {
		// An MR shipping a CRD with spec.keepObjects unset is the destructive
		// misconfiguration we want to surface. The reconcile still stamps the
		// CRD (working webhook > principled refusal), but the violations
		// counter advances and an error-level log is emitted.
		s := testScheme(t)
		docs := [][]byte{crdDocServiceForm(t)}
		raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
			Data:       map[string][]byte{"objects.yaml": raw},
		}
		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})
		mr.Spec.KeepObjects = nil // explicit opt-out of the makeMR default

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret(), secret).
			Build()
		cc := &countingClient{Client: base}

		before := counterValue(t, metrics.ManagedResourceKeepObjectsViolationsTotal)
		reconcileOnce(t, cc)
		after := counterValue(t, metrics.ManagedResourceKeepObjectsViolationsTotal)

		if got := after - before; got != 1 {
			t.Fatalf("expected keep_objects_violations_total +1, got +%v", got)
		}
		// Reconcile must still stamp despite the violation.
		if cc.updates != 1 {
			t.Fatalf("expected exactly 1 Secret Update even with violation, got %d", cc.updates)
		}
	})

	t.Run("KeepObjectsViolationWhenExplicitlyFalse", func(t *testing.T) {
		// keepObjects=false is the same destructive shape as keepObjects unset;
		// Gardener treats nil and false identically. The violation must fire
		// in both cases.
		s := testScheme(t)
		docs := [][]byte{crdDocServiceForm(t)}
		raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
			Data:       map[string][]byte{"objects.yaml": raw},
		}
		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})
		f := false
		mr.Spec.KeepObjects = &f

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret(), secret).
			Build()
		cc := &countingClient{Client: base}

		before := counterValue(t, metrics.ManagedResourceKeepObjectsViolationsTotal)
		reconcileOnce(t, cc)
		after := counterValue(t, metrics.ManagedResourceKeepObjectsViolationsTotal)

		if got := after - before; got != 1 {
			t.Fatalf("expected keep_objects_violations_total +1, got +%v", got)
		}
	})

	t.Run("NoKeepObjectsViolationWhenMRHasNoCRDs", func(t *testing.T) {
		// An MR that ships only ConfigMaps / Deployments / etc. is not subject
		// to the keepObjects=true invariant. Unset keepObjects must NOT trip
		// the violation counter here — the metric is "CRD-bearing MRs without
		// the flag", not "all MRs without the flag".
		s := testScheme(t)
		docs := [][]byte{nonCRDDoc("a"), nonCRDDoc("b")}
		raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
			Data:       map[string][]byte{"objects.yaml": raw},
		}
		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})
		mr.Spec.KeepObjects = nil

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret(), secret).
			Build()
		cc := &countingClient{Client: base}

		before := counterValue(t, metrics.ManagedResourceKeepObjectsViolationsTotal)
		reconcileOnce(t, cc)
		after := counterValue(t, metrics.ManagedResourceKeepObjectsViolationsTotal)

		if got := after - before; got != 0 {
			t.Fatalf("expected no violation when MR has no CRDs, got +%v", got)
		}
	})

	t.Run("NoKeepObjectsViolationWhenTrue", func(t *testing.T) {
		// The happy path: CRD-bearing MR with spec.keepObjects=true (the
		// default set by makeMR). Violation counter must not advance.
		s := testScheme(t)
		docs := [][]byte{crdDocServiceForm(t)}
		raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
			Data:       map[string][]byte{"objects.yaml": raw},
		}
		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})
		// makeMR already sets KeepObjects=true; assertion just for clarity.
		if mr.Spec.KeepObjects == nil || !*mr.Spec.KeepObjects {
			t.Fatalf("test setup bug: makeMR should set KeepObjects=true")
		}

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret(), secret).
			Build()
		cc := &countingClient{Client: base}

		before := counterValue(t, metrics.ManagedResourceKeepObjectsViolationsTotal)
		reconcileOnce(t, cc)
		after := counterValue(t, metrics.ManagedResourceKeepObjectsViolationsTotal)

		if got := after - before; got != 0 {
			t.Fatalf("expected no violation when keepObjects=true, got +%v", got)
		}
	})

	t.Run("ConditionUnhealthyFalseIsObserved", func(t *testing.T) {
		// MR with ResourcesApplied=False and ResourcesHealthy=Progressing
		// must produce exactly two counter increments — one per condition —
		// each with the matching status label, and the reconcile must still
		// stamp the CRD (health observation is informational, not blocking).
		s := testScheme(t)
		docs := [][]byte{crdDocServiceForm(t)}
		raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
			Data:       map[string][]byte{"objects.yaml": raw},
		}
		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})
		mr.Status.Conditions = []gardencorev1beta1.Condition{
			{Type: conditionResourcesApplied, Status: "False", Reason: "ApplyFailed", Message: "kubectl apply returned error"},
			{Type: conditionResourcesHealthy, Status: "Progressing", Reason: "PodsRolling", Message: "deployment is still rolling out"},
		}

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret(), secret).
			WithStatusSubresource(&resourcesv1alpha1.ManagedResource{}).
			Build()
		cc := &countingClient{Client: base}

		appliedFalseBefore := counterValue(t, metrics.ManagedResourceUnhealthyTotal.WithLabelValues(conditionResourcesApplied, "False"))
		healthyProgressingBefore := counterValue(t, metrics.ManagedResourceUnhealthyTotal.WithLabelValues(conditionResourcesHealthy, "Progressing"))

		reconcileOnce(t, cc)

		if got := counterValue(t, metrics.ManagedResourceUnhealthyTotal.WithLabelValues(conditionResourcesApplied, "False")) - appliedFalseBefore; got != 1 {
			t.Errorf("expected unhealthy_total{condition=ResourcesApplied,status=False} +1, got +%v", got)
		}
		if got := counterValue(t, metrics.ManagedResourceUnhealthyTotal.WithLabelValues(conditionResourcesHealthy, "Progressing")) - healthyProgressingBefore; got != 1 {
			t.Errorf("expected unhealthy_total{condition=ResourcesHealthy,status=Progressing} +1, got +%v", got)
		}
		// Stamp still happened.
		if cc.updates != 1 {
			t.Errorf("expected stamp despite unhealthy condition, got updates=%d", cc.updates)
		}
	})

	t.Run("ConditionHealthyTrueIsSilent", func(t *testing.T) {
		// Both conditions=True must not increment the counter for any label
		// combination tracked here.
		s := testScheme(t)
		docs := [][]byte{crdDocServiceForm(t)}
		raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
			Data:       map[string][]byte{"objects.yaml": raw},
		}
		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})
		mr.Status.Conditions = []gardencorev1beta1.Condition{
			{Type: conditionResourcesApplied, Status: "True"},
			{Type: conditionResourcesHealthy, Status: "True"},
		}

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret(), secret).
			WithStatusSubresource(&resourcesv1alpha1.ManagedResource{}).
			Build()
		cc := &countingClient{Client: base}

		// Sum across the tracked label combinations so we don't have to
		// enumerate every status here.
		const statusFalse, statusProgressing, statusUnknown = "False", "Progressing", "Unknown"
		startSum := 0.0
		for _, cond := range []string{conditionResourcesApplied, conditionResourcesHealthy} {
			for _, st := range []string{statusFalse, statusProgressing, statusUnknown} {
				startSum += counterValue(t, metrics.ManagedResourceUnhealthyTotal.WithLabelValues(cond, st))
			}
		}

		reconcileOnce(t, cc)

		endSum := 0.0
		for _, cond := range []string{conditionResourcesApplied, conditionResourcesHealthy} {
			for _, st := range []string{statusFalse, statusProgressing, statusUnknown} {
				endSum += counterValue(t, metrics.ManagedResourceUnhealthyTotal.WithLabelValues(cond, st))
			}
		}
		if endSum != startSum {
			t.Fatalf("expected zero unhealthy increments when both conditions are True, sum delta = %v", endSum-startSum)
		}
	})

	t.Run("MissingConditionsAreSilent", func(t *testing.T) {
		// A freshly created MR with no status yet must NOT trip the
		// unhealthy counter — "missing" is not the same as "False".
		s := testScheme(t)
		docs := [][]byte{crdDocServiceForm(t)}
		raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
			Data:       map[string][]byte{"objects.yaml": raw},
		}
		mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})
		// mr.Status.Conditions intentionally nil

		base := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(mr, makeCertSecret(), secret).
			WithStatusSubresource(&resourcesv1alpha1.ManagedResource{}).
			Build()
		cc := &countingClient{Client: base}

		startSum := 0.0
		for _, cond := range []string{conditionResourcesApplied, conditionResourcesHealthy} {
			for _, st := range []string{"False", "Progressing", "Unknown"} {
				startSum += counterValue(t, metrics.ManagedResourceUnhealthyTotal.WithLabelValues(cond, st))
			}
		}

		reconcileOnce(t, cc)

		endSum := 0.0
		for _, cond := range []string{conditionResourcesApplied, conditionResourcesHealthy} {
			for _, st := range []string{"False", "Progressing", "Unknown"} {
				endSum += counterValue(t, metrics.ManagedResourceUnhealthyTotal.WithLabelValues(cond, st))
			}
		}
		if endSum != startSum {
			t.Fatalf("expected zero unhealthy increments when conditions are missing, sum delta = %v", endSum-startSum)
		}
	})
}

// TestManagedResourceReconcilerMetrics asserts that a single reconcile that
// stamps a CRD increments the expected counters. We don't try to test every
// metric in every code path — the reconcile contract is exercised in the
// suite above; this is just enough to catch a regression that removes the
// metrics import or silently drops an increment.
func TestManagedResourceReconcilerMetrics(t *testing.T) {
	s := testScheme(t)
	docs := [][]byte{crdDocServiceForm(t)}
	raw, err := managedresource.Encode(managedresource.JoinDocs(docs), false)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: mrNamespace},
		Data:       map[string][]byte{"objects.yaml": raw},
	}
	mr := makeMR(map[string]string{webhookLabelKey: webhookLabelValue})

	base := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(mr, makeCertSecret(), secret).
		Build()
	cc := &countingClient{Client: base}

	// Snapshot counters before reconcile so we can assert deltas.
	before := struct {
		stampedResult         float64
		crdsStamped           float64
		secretKeysPlain       float64
		forceReconcilePatched float64
	}{
		stampedResult:         counterValue(t, metrics.ManagedResourceStampsTotal.WithLabelValues(metrics.ResultStamped)),
		crdsStamped:           counterValue(t, metrics.ManagedResourceCRDsStampedTotal),
		secretKeysPlain:       counterValue(t, metrics.ManagedResourceSecretKeysTotal.WithLabelValues(metrics.EncodingPlain)),
		forceReconcilePatched: counterValue(t, metrics.ManagedResourceForceReconcileTotal.WithLabelValues(metrics.ForceReconcilePatched)),
	}

	reconcileOnce(t, cc)

	// Delta assertions. Counters are process-wide so we compare against the
	// snapshot rather than expecting absolute values — multiple tests in
	// the same package share the registry.
	if got := counterValue(t, metrics.ManagedResourceStampsTotal.WithLabelValues(metrics.ResultStamped)) - before.stampedResult; got != 1 {
		t.Errorf("expected stamps_total{result=stamped} +1, got +%v", got)
	}
	if got := counterValue(t, metrics.ManagedResourceCRDsStampedTotal) - before.crdsStamped; got != 1 {
		t.Errorf("expected crds_stamped_total +1, got +%v", got)
	}
	if got := counterValue(t, metrics.ManagedResourceSecretKeysTotal.WithLabelValues(metrics.EncodingPlain)) - before.secretKeysPlain; got != 1 {
		t.Errorf("expected secret_keys_total{encoding=plain} +1, got +%v", got)
	}
	if got := counterValue(t, metrics.ManagedResourceForceReconcileTotal.WithLabelValues(metrics.ForceReconcilePatched)) - before.forceReconcilePatched; got != 1 {
		t.Errorf("expected force_reconcile_total{result=patched} +1, got +%v", got)
	}
}

// counterValue reads the current value of a Prometheus counter via the
// generic dto channel. Used by metrics tests so we don't have to reach for
// promtestutil and pull a new dep.
func counterValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	close(ch)
	m, ok := <-ch
	if !ok {
		return 0 // counter never incremented and never observed
	}
	var pb dto.Metric
	if err := m.Write(&pb); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if pb.Counter == nil {
		t.Fatalf("not a counter: %+v", &pb)
	}
	return pb.Counter.GetValue()
}

// TestLabelPredicate covers every branch of the watch predicate that
// gates ManagedResource events into the reconciler. The four cases that
// must pass: Create/Delete/Generic of a labeled MR, and Update where
// `spec.generation` actually changed. The cases that must be filtered:
// Update of a labeled MR where only `status` or `metadata` (annotations)
// changed — these are the noisy GRM status flips and our own
// force-reconcile annotation echo that motivated the predicate. Plus
// two compositional cases: an MR newly entering scope (label added) is
// admitted even without a generation bump; an MR leaving scope (label
// removed) is filtered.
func TestLabelPredicate(t *testing.T) {
	r := &ManagedResourceReconciler{
		WebhookLabelKey:   "k",
		WebhookLabelValue: "v",
	}
	p := r.labelPredicate()

	labeled := func(name string, gen int64, annotations map[string]string) *resourcesv1alpha1.ManagedResource {
		return &resourcesv1alpha1.ManagedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   "ns",
				Labels:      map[string]string{"k": "v"},
				Annotations: annotations,
				Generation:  gen,
			},
		}
	}
	unlabeled := func(name string, gen int64) *resourcesv1alpha1.ManagedResource {
		return &resourcesv1alpha1.ManagedResource{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Namespace:  "ns",
				Generation: gen,
			},
		}
	}

	t.Run("CreateOfLabeledPasses", func(t *testing.T) {
		if !p.Create(event.CreateEvent{Object: labeled("mr", 1, nil)}) {
			t.Fatal("Create of labeled MR was filtered")
		}
	})
	t.Run("CreateOfUnlabeledFiltered", func(t *testing.T) {
		if p.Create(event.CreateEvent{Object: unlabeled("mr", 1)}) {
			t.Fatal("Create of unlabeled MR leaked through")
		}
	})
	t.Run("DeleteOfLabeledPasses", func(t *testing.T) {
		if !p.Delete(event.DeleteEvent{Object: labeled("mr", 1, nil)}) {
			t.Fatal("Delete of labeled MR was filtered")
		}
	})
	t.Run("GenericOfLabeledPasses", func(t *testing.T) {
		if !p.Generic(event.GenericEvent{Object: labeled("mr", 1, nil)}) {
			t.Fatal("Generic of labeled MR was filtered")
		}
	})

	t.Run("UpdateWithGenerationChangePasses", func(t *testing.T) {
		old := labeled("mr", 1, nil)
		nw := labeled("mr", 2, nil)
		if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: nw}) {
			t.Fatal("Update with generation bump was filtered")
		}
	})

	t.Run("UpdateStatusOnlyFiltered", func(t *testing.T) {
		// Same generation, same labels, same annotations — GRM rewrote
		// status. This is the noisy case the predicate exists to suppress.
		old := labeled("mr", 5, nil)
		nw := labeled("mr", 5, nil)
		if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: nw}) {
			t.Fatal("status-only Update leaked through")
		}
	})

	t.Run("UpdateAnnotationOnlyFiltered", func(t *testing.T) {
		// Same generation; our own force-reconcile annotation patch
		// arrives back as a watch event. Must not retrigger reconcile.
		old := labeled("mr", 5, nil)
		nw := labeled("mr", 5, map[string]string{"gardener.cloud/operation": "reconcile"})
		if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: nw}) {
			t.Fatal("annotation-only Update leaked through")
		}
	})

	t.Run("UpdateLabelAddedPasses", func(t *testing.T) {
		// The MR was outside our scope and just acquired our label.
		// Functionally a Create — admit even without a generation bump.
		old := unlabeled("mr", 5)
		nw := labeled("mr", 5, nil)
		if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: nw}) {
			t.Fatal("label-added Update was filtered")
		}
	})

	t.Run("UpdateLabelRemovedFiltered", func(t *testing.T) {
		// The reverse: MR left our scope. Filter — the new object
		// fails the label check, so we don't care what's in it.
		old := labeled("mr", 5, nil)
		nw := unlabeled("mr", 5)
		if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: nw}) {
			t.Fatal("label-removed Update leaked through")
		}
	})
}
