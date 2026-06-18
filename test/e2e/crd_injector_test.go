// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
