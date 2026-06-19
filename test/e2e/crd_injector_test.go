// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/certificates"
)

// TestCRDInjector exercises cmd/crd-injector end-to-end: create a ConfigMap
// holding two CRDs in the hosting cluster, run the binary against the target
// cluster's kubeconfig, and assert both CRDs exist on the target cluster.
//
// The test runs the binary directly via `go run` rather than deploying it as a
// Job inside the hosting cluster. That avoids needing a rebuilt image, RBAC
// manifests, or a Job spec — this binary is meant to be invoked from outside
// the apiserver flow anyway, so a local invocation is a faithful smoke test.
//
// Assumes setup.sh has run (or --test.reuse-clusters=true): both kind clusters
// must exist with contexts kind-hosting / kind-target. Reuses the cluster
// lifecycle gating from TestWebhookLoadAndRotation so devs can run both tests
// in one shot without paying the setup cost twice.
func TestCRDInjector(t *testing.T) {
	// Use an absolute path. The crd-injector subprocess below runs with
	// cmd.Dir set to the repo root (so `go run <module-path>` resolves), so
	// any relative kubeconfig path would be looked up there instead of in
	// test/e2e/. CI hit exactly this — `stat kubeconfig: no such file`.
	kubeconfigPath, absErr := filepath.Abs(filepath.Join(".", "kubeconfig"))
	if absErr != nil {
		t.Fatalf("Resolve kubeconfig path: %v", absErr)
	}
	t.Setenv("KUBECONFIG", kubeconfigPath)

	if !*reuseClusters {
		logWithTimestamp(t, "Setting up clusters for crd-injector test...")
		if err := runCommand(t, "./setup.sh"); err != nil {
			t.Fatalf("Setup failed: %v", err)
		}
		t.Cleanup(func() {
			logWithTimestamp(t, "Tearing down clusters...")
			runCommand(t, "./teardown.sh") //nolint:errcheck // cleanup
		})
	} else {
		logWithTimestamp(t, "Reusing existing clusters (skipping setup/teardown)")
	}

	hostingClient := getKubernetesClient(t, "kind-hosting")
	const (
		ns      = "default"
		cmName  = "crd-injector-e2e-crds"
		crdName = "crdinjectortests.example.com"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Recreate the source ConfigMap so reruns under -test.reuse-clusters start
	// from a clean slate.
	_ = hostingClient.CoreV1().ConfigMaps(ns).Delete(ctx, cmName, metav1.DeleteOptions{}) //nolint:errcheck
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns},
		Data: map[string]string{
			"crd.yaml": fmt.Sprintf(crdYAMLTemplate, crdName, "CrdInjectorTest", "CrdInjectorTestList", "crdinjectortests", "crdinjectortest"),
		},
	}
	if _, err := hostingClient.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create source ConfigMap: %v", err)
	}
	t.Cleanup(func() {
		_ = hostingClient.CoreV1().ConfigMaps(ns).Delete(context.Background(), cmName, metav1.DeleteOptions{}) //nolint:errcheck
	})

	// Make sure no leftover from a previous run lives on the target cluster —
	// we want to assert crd-injector created the CRD this run.
	targetCfg := loadContextConfig(t, kubeconfigPath, "kind-target")
	apiextClient, err := apiextclient.NewForConfig(targetCfg)
	if err != nil {
		t.Fatalf("Build apiextensions client for target: %v", err)
	}
	_ = apiextClient.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, crdName, metav1.DeleteOptions{}) //nolint:errcheck

	// Run the binary. Use `go run` so we always exercise the current source
	// tree without a pre-build step. Both clusters share one kubeconfig file —
	// crd-injector reads --target-kubeconfig for the target and uses
	// KUBECONFIG (with a forced context override via env) for the hosting
	// cluster client. controller-runtime's GetConfig honors the standard
	// client-go loading rules, which means it picks up KUBECONFIG and the
	// current-context — so we must point current-context at kind-hosting for
	// the duration of the run.
	hostingKubeconfig := writeContextKubeconfig(t, kubeconfigPath, "kind-hosting")

	logWithTimestamp(t, "Running crd-injector...")
	cmd := exec.Command("go", "run", //nolint:gosec // test-controlled args
		"github.com/sap-cloud-infrastructure/webhook-injector/cmd/crd-injector",
		"--configmap-name="+cmName,
		"--namespace="+ns,
		"--target-kubeconfig="+kubeconfigPath, // this kubeconfig has both contexts
	)
	cmd.Dir = filepath.Join("..", "..") // run from repo root so module is resolvable
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+hostingKubeconfig,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("crd-injector exited non-zero: %v", err)
	}

	// Poll the target cluster for the CRD. Apply is synchronous on the
	// apiserver, so this should resolve on the first iteration; the loop is
	// just a guard against transient cache lag.
	deadline := time.Now().Add(15 * time.Second)
	var got *apiextensionsv1.CustomResourceDefinition
	for {
		got, err = apiextClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
		if err == nil {
			break
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("Get CRD on target: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("CRD %s not found on target cluster after apply: %v", crdName, err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	if got.Spec.Group != "example.com" {
		t.Errorf("CRD spec.group = %q, want example.com", got.Spec.Group)
	}
	if mgrs := got.GetManagedFields(); len(mgrs) == 0 || mgrs[0].Manager != "crd-injector" {
		// Server-side apply records the field manager. Anything other than
		// "crd-injector" indicates the binary's --field-manager default
		// changed or the apply path went somewhere unexpected.
		t.Errorf("CRD field manager = %v, want first entry to be crd-injector", mgrs)
	}

	// Re-run idempotency check: a second invocation must succeed and not
	// flip ownership or fail with a conflict.
	logWithTimestamp(t, "Re-running crd-injector (idempotency check)...")
	cmd2 := exec.Command("go", "run", //nolint:gosec // test-controlled args
		"github.com/sap-cloud-infrastructure/webhook-injector/cmd/crd-injector",
		"--configmap-name="+cmName,
		"--namespace="+ns,
		"--target-kubeconfig="+kubeconfigPath,
	)
	cmd2.Dir = filepath.Join("..", "..")
	cmd2.Env = append(os.Environ(), "KUBECONFIG="+hostingKubeconfig)
	cmd2.Stdout = os.Stdout
	cmd2.Stderr = os.Stderr
	if err := cmd2.Run(); err != nil {
		t.Fatalf("crd-injector second invocation failed: %v", err)
	}

	t.Cleanup(func() {
		_ = apiextClient.ApiextensionsV1().CustomResourceDefinitions().Delete(context.Background(), crdName, metav1.DeleteOptions{}) //nolint:errcheck
	})
}

// TestCRDInjectorPreservesWebhookInjectorFields exercises the strip-on-update
// behaviour of cmd/crd-injector: it must not fight whatever webhook-injector
// has stamped into spec.conversion.webhook.clientConfig.{caBundle,url,service}.
//
// We deliberately do *not* run the full webhook-injector controller here.
// The property under test is purely about crd-injector's apply body: after
// the CRD already exists, those three fields must be absent from the body
// (so SSA does not declare ownership), and any caBundle / URL / Service
// state on the live object must survive a re-apply unchanged. We simulate
// webhook-injector's stamping with a direct PATCH between the two
// crd-injector runs — a faithful stand-in, since webhook-injector's actual
// effect on the CRD would be a Patch with a different fieldOwner that
// produces the same on-cluster shape.
func TestCRDInjectorPreservesWebhookInjectorFields(t *testing.T) {
	kubeconfigPath, absErr := filepath.Abs(filepath.Join(".", "kubeconfig"))
	if absErr != nil {
		t.Fatalf("Resolve kubeconfig path: %v", absErr)
	}
	t.Setenv("KUBECONFIG", kubeconfigPath)

	if !*reuseClusters {
		logWithTimestamp(t, "Setting up clusters for crd-injector preserve test...")
		if err := runCommand(t, "./setup.sh"); err != nil {
			t.Fatalf("Setup failed: %v", err)
		}
		t.Cleanup(func() {
			logWithTimestamp(t, "Tearing down clusters...")
			runCommand(t, "./teardown.sh") //nolint:errcheck // cleanup
		})
	} else {
		logWithTimestamp(t, "Reusing existing clusters (skipping setup/teardown)")
	}

	hostingClient := getKubernetesClient(t, "kind-hosting")
	const (
		ns      = "default"
		cmName  = "crd-injector-preserve-e2e-crds"
		crdName = "preservetests.example.com"
		// Value that simulates what webhook-injector would write into
		// clientConfig.url after the Service→URL rewrite.
		stampedURL = "https://webhook.example.invalid:9443/convert"
	)

	// Generate a real self-signed PEM cert to use as the simulated
	// caBundle: the apiserver validates that spec.conversion.webhook.clientConfig.caBundle
	// parses as a PEM block, so a hardcoded placeholder is rejected.
	stampedCABundle, _, err := certificates.GenerateSelfSignedCert(
		[]certificates.ServiceReference{{Namespace: "default", Name: "preserve-conversion-svc"}},
		1*time.Hour,
	)
	if err != nil {
		t.Fatalf("Generate stamp caBundle: %v", err)
	}
	stampedCABundleB64 := base64.StdEncoding.EncodeToString(stampedCABundle)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Recreate the source ConfigMap so reruns under -test.reuse-clusters start
	// from a clean slate. The user-authored body uses the natural service-
	// form clientConfig.
	_ = hostingClient.CoreV1().ConfigMaps(ns).Delete(ctx, cmName, metav1.DeleteOptions{}) //nolint:errcheck
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns},
		Data: map[string]string{
			"crd.yaml": fmt.Sprintf(crdConversionYAMLTemplate,
				crdName,                   // metadata.name
				"PreserveTest",            // names.kind
				"PreserveTestList",        // names.listKind
				"preservetests",           // names.plural
				"preservetest",            // names.singular
				"default",                 // service.namespace
				"preserve-conversion-svc", // service.name
				"/convert",                // service.path
				"v1",                      // first version name
				"v2",                      // second version name
			),
		},
	}
	if _, err := hostingClient.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Create source ConfigMap: %v", err)
	}
	t.Cleanup(func() {
		_ = hostingClient.CoreV1().ConfigMaps(ns).Delete(context.Background(), cmName, metav1.DeleteOptions{}) //nolint:errcheck
	})

	targetCfg := loadContextConfig(t, kubeconfigPath, "kind-target")
	apiextClient, apiextErr := apiextclient.NewForConfig(targetCfg)
	if apiextErr != nil {
		t.Fatalf("Build apiextensions client for target: %v", apiextErr)
	}
	_ = apiextClient.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, crdName, metav1.DeleteOptions{}) //nolint:errcheck
	t.Cleanup(func() {
		_ = apiextClient.ApiextensionsV1().CustomResourceDefinitions().Delete(context.Background(), crdName, metav1.DeleteOptions{}) //nolint:errcheck
	})

	hostingKubeconfig := writeContextKubeconfig(t, kubeconfigPath, "kind-hosting")

	// ----- First crd-injector run: creates the CRD with the user-authored
	// service-form clientConfig.
	logWithTimestamp(t, "Running crd-injector (first run, create)...")
	if err := runCRDInjector(t, kubeconfigPath, hostingKubeconfig, cmName, ns); err != nil {
		t.Fatalf("crd-injector first run: %v", err)
	}

	// Wait for the CRD to appear and confirm initial shape.
	got := waitForCRD(t, ctx, apiextClient, crdName)
	if conv := got.Spec.Conversion; conv == nil ||
		conv.Strategy != apiextensionsv1.WebhookConverter ||
		conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		t.Fatalf("CRD missing Webhook conversion clientConfig after first run: %+v", got.Spec.Conversion)
	}
	if got.Spec.Conversion.Webhook.ClientConfig.Service == nil {
		t.Fatalf("First run: expected service-form clientConfig from user-authored body, got: %+v",
			got.Spec.Conversion.Webhook.ClientConfig)
	}

	// ----- Simulate webhook-injector stamping: set caBundle, rewrite to URL
	// form, clear service. We use a JSON patch with a different fieldManager
	// so the resulting managed-fields layout mirrors what webhook-injector
	// produces on its own Update path.
	stampPatch := fmt.Appendf(nil,
		`[{"op":"replace","path":"/spec/conversion/webhook/clientConfig","value":{"caBundle":"%s","url":"%s"}}]`,
		stampedCABundleB64, stampedURL,
	)
	if _, err := apiextClient.ApiextensionsV1().CustomResourceDefinitions().Patch(
		ctx, crdName, types.JSONPatchType, stampPatch,
		metav1.PatchOptions{FieldManager: "simulated-webhook-injector"},
	); err != nil {
		t.Fatalf("Simulate webhook-injector stamp: %v", err)
	}

	// Read it back and confirm the simulated stamp landed.
	got, err = apiextClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get CRD after simulated stamp: %v", err)
	}
	cc := got.Spec.Conversion.Webhook.ClientConfig
	if cc.URL == nil || *cc.URL != stampedURL {
		t.Fatalf("After simulated stamp: clientConfig.URL = %v, want %s", cc.URL, stampedURL)
	}
	if cc.Service != nil {
		t.Fatalf("After simulated stamp: clientConfig.Service should be cleared, got %+v", cc.Service)
	}
	if !bytes.Equal(cc.CABundle, stampedCABundle) {
		t.Fatalf("After simulated stamp: clientConfig.CABundle does not match the stamped value")
	}

	// ----- Second crd-injector run: must NOT revert the stamped fields.
	logWithTimestamp(t, "Running crd-injector (second run, must preserve stamped fields)...")
	if err := runCRDInjector(t, kubeconfigPath, hostingKubeconfig, cmName, ns); err != nil {
		t.Fatalf("crd-injector second run: %v", err)
	}

	got, err = apiextClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get CRD after second run: %v", err)
	}
	cc = got.Spec.Conversion.Webhook.ClientConfig
	if cc.URL == nil || *cc.URL != stampedURL {
		t.Errorf("After re-apply: clientConfig.URL = %v, want %s (crd-injector reverted webhook-injector's rewrite)",
			cc.URL, stampedURL)
	}
	if cc.Service != nil {
		t.Errorf("After re-apply: clientConfig.Service should still be cleared, got %+v (crd-injector reverted to user-authored service-form)",
			cc.Service)
	}
	if !bytes.Equal(cc.CABundle, stampedCABundle) {
		t.Errorf("After re-apply: clientConfig.CABundle was changed by crd-injector; live=%q stamped=%q",
			string(cc.CABundle), string(stampedCABundle))
	}

	// ----- Managed-fields must show that crd-injector does not own
	// caBundle, url, or service under spec.conversion.webhook.clientConfig.
	// With server-side apply, fields absent from the apply body are not
	// owned by the applier; the strip ensures none of those three keys
	// appear in crd-injector's fieldsV1 entry.
	mf := findManagedFields(got.GetManagedFields(), "crd-injector")
	if mf == nil {
		t.Fatalf("No managed-fields entry for fieldOwner crd-injector; got managers: %v",
			managerNames(got.GetManagedFields()))
	}
	if mf.FieldsV1 == nil || len(mf.FieldsV1.Raw) == 0 {
		t.Fatalf("crd-injector managed-fields entry has empty FieldsV1")
	}
	for _, forbidden := range []string{`"f:caBundle"`, `"f:url"`, `"f:service"`} {
		if strings.Contains(string(mf.FieldsV1.Raw), forbidden) {
			t.Errorf("crd-injector should not own %s under spec.conversion.webhook.clientConfig; FieldsV1=%s",
				forbidden, string(mf.FieldsV1.Raw))
		}
	}
}

// runCRDInjector invokes cmd/crd-injector via `go run` against the shared
// kubeconfig with current-context pinned to kind-hosting. Mirrors the
// invocation in TestCRDInjector.
func runCRDInjector(t *testing.T, kubeconfigPath, hostingKubeconfig, cmName, ns string) error {
	t.Helper()
	cmd := exec.Command("go", "run", //nolint:gosec // test-controlled args
		"github.com/sap-cloud-infrastructure/webhook-injector/cmd/crd-injector",
		"--configmap-name="+cmName,
		"--namespace="+ns,
		"--target-kubeconfig="+kubeconfigPath,
	)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+hostingKubeconfig)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// waitForCRD polls the apiserver until the named CRD exists or the timeout
// in ctx fires. Apply is synchronous on the apiserver, so this typically
// resolves on the first iteration; the loop is just a guard against
// transient cache lag.
func waitForCRD(t *testing.T, ctx context.Context, c *apiextclient.Clientset, name string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := c.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return got
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("Get CRD %s: %v", name, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("CRD %s not found within timeout: %v", name, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// findManagedFields returns the first managed-fields entry whose Manager
// equals the given name, or nil if absent.
func findManagedFields(entries []metav1.ManagedFieldsEntry, manager string) *metav1.ManagedFieldsEntry {
	for i := range entries {
		if entries[i].Manager == manager {
			return &entries[i]
		}
	}
	return nil
}

// managerNames extracts the Manager strings from managed-fields entries.
// Used only for clearer test failure messages.
func managerNames(entries []metav1.ManagedFieldsEntry) []string {
	out := make([]string, len(entries))
	for i := range entries {
		out[i] = entries[i].Manager
	}
	return out
}

// crdYAMLTemplate is a minimal valid v1 CRD. Format args (in order):
// metadata.name, spec.names.kind, listKind, plural, singular.
const crdYAMLTemplate = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: %s
spec:
  group: example.com
  names:
    kind: %s
    listKind: %s
    plural: %s
    singular: %s
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
`

// crdConversionYAMLTemplate is a v1 CRD with a Webhook conversion strategy
// pointing at a Service. Mirrors the way users typically author CRDs whose
// conversion webhooks are served by a sidecar in the target cluster.
//
// Format args (in order): metadata.name, names.kind, names.listKind,
// names.plural, names.singular, two version names (v1, v2),
// webhook service namespace, service name, service path.
const crdConversionYAMLTemplate = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: %s
spec:
  group: example.com
  names:
    kind: %s
    listKind: %s
    plural: %s
    singular: %s
  scope: Namespaced
  conversion:
    strategy: Webhook
    webhook:
      conversionReviewVersions: ["v1"]
      clientConfig:
        service:
          namespace: %s
          name: %s
          path: %s
  versions:
    - name: %s
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
    - name: %s
      served: true
      storage: false
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
`

// loadContextConfig returns a *rest.Config bound to a specific context within
// the shared kubeconfig file.
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

// writeContextKubeconfig copies the shared kubeconfig to a temp file and pins
// its current-context to the given context. crd-injector resolves its
// hosting-cluster client via controller-runtime's ctrl.GetConfig(), which
// honors KUBECONFIG and the current-context selector — so to drive it at
// kind-hosting we point it at a kubeconfig file whose current-context is
// already that.
func writeContextKubeconfig(t *testing.T, srcPath, contextName string) string {
	t.Helper()
	cfg, err := clientcmd.LoadFromFile(srcPath)
	if err != nil {
		t.Fatalf("Load kubeconfig %s: %v", srcPath, err)
	}
	cfg.CurrentContext = contextName
	dst := filepath.Join(t.TempDir(), "kubeconfig-"+contextName)
	if err := clientcmd.WriteToFile(*cfg, dst); err != nil {
		t.Fatalf("Write kubeconfig %s: %v", dst, err)
	}
	return dst
}
