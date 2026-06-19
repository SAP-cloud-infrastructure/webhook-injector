// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

// crd-injector reads a ConfigMap from the hosting cluster, parses every value as
// (multi-document) YAML containing CustomResourceDefinitions, and server-side
// applies each CRD to the target cluster. It exits 0 on success and non-zero on
// any parse or apply error.
//
// It is intentionally minimal: no controller, no watches, no caBundle handling.
// The expectation is that webhook-injector's labeled-CRD path stamps caBundle
// (and the Service→URL rewrite) on these CRDs after they exist; to avoid
// fighting webhook-injector across re-applies, applyCRD strips the fields
// webhook-injector owns from the apply body when the CRD already exists.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	scheme       = runtime.NewScheme()
	codecFactory serializer.CodecFactory
	setupLog     = ctrl.Log.WithName("crd-injector")
)

var (
	configMapName    string
	namespace        string
	targetKubeconfig string
	fieldManager     string
	showVersion      bool
)

func init() {
	flag.StringVar(&configMapName, "configmap-name", "", "ConfigMap (in --namespace, hosting cluster) whose values contain CustomResourceDefinition YAML. Required.")
	flag.StringVar(&namespace, "namespace", "", "Namespace of the ConfigMap (auto-detects from NAMESPACE env or pod's serviceaccount file when unset).")
	flag.StringVar(&targetKubeconfig, "target-kubeconfig", "", "Path to target cluster kubeconfig. Defaults to in-cluster config.")
	flag.StringVar(&fieldManager, "field-manager", "crd-injector", "fieldManager value used for server-side apply.")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit.")

	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	codecFactory = serializer.NewCodecFactory(scheme)
}

func main() {
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if showVersion {
		fmt.Println("crd-injector")
		return
	}

	if configMapName == "" {
		setupLog.Error(nil, "--configmap-name is required")
		os.Exit(1)
	}

	if namespace == "" {
		ns, err := detectNamespace()
		if err != nil {
			setupLog.Error(err, "Failed to detect namespace")
			os.Exit(1)
		}
		namespace = ns
	}

	ctx := ctrl.SetupSignalHandler()

	hostingClient, err := buildHostingClient()
	if err != nil {
		setupLog.Error(err, "Failed to build hosting cluster client")
		os.Exit(1)
	}
	targetClient, err := buildTargetClient()
	if err != nil {
		setupLog.Error(err, "Failed to build target cluster client")
		os.Exit(1)
	}

	cm := &corev1.ConfigMap{}
	if err := hostingClient.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: namespace}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			setupLog.Error(err, "ConfigMap not found", "namespace", namespace, "name", configMapName)
		} else {
			setupLog.Error(err, "Failed to read ConfigMap", "namespace", namespace, "name", configMapName)
		}
		os.Exit(1)
	}

	crds, err := parseCRDs(cm)
	if err != nil {
		setupLog.Error(err, "Failed to parse CRDs from ConfigMap")
		os.Exit(1)
	}
	if len(crds) == 0 {
		setupLog.Info("ConfigMap contained no CustomResourceDefinitions, nothing to apply",
			"namespace", namespace, "name", configMapName)
		return
	}

	for _, crd := range crds {
		if err := applyCRD(ctx, targetClient, crd); err != nil {
			setupLog.Error(err, "Failed to apply CRD", "name", crd.Name)
			os.Exit(1)
		}
		setupLog.Info("Applied CRD", "name", crd.Name)
	}

	setupLog.Info("All CRDs applied", "count", len(crds))
}

// parseCRDs walks every key of the ConfigMap (in deterministic order) and
// decodes each value as a multi-document YAML stream. Every decoded document
// must be a v1 CustomResourceDefinition; anything else is an error so this
// binary cannot accidentally be used as a generic applier.
func parseCRDs(cm *corev1.ConfigMap) ([]*apiextensionsv1.CustomResourceDefinition, error) {
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []*apiextensionsv1.CustomResourceDefinition
	for _, key := range keys {
		decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(cm.Data[key])), 4096)
		for docIdx := 0; ; docIdx++ {
			raw := &runtime.Unknown{}
			if err := decoder.Decode(raw); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("decode %s[doc %d]: %w", key, docIdx, err)
			}
			if len(bytes.TrimSpace(raw.Raw)) == 0 {
				continue
			}
			obj, gvk, err := codecFactory.UniversalDeserializer().Decode(raw.Raw, nil, nil)
			if err != nil {
				return nil, fmt.Errorf("decode %s[doc %d]: %w", key, docIdx, err)
			}
			crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				return nil, fmt.Errorf("decode %s[doc %d]: expected apiextensions.k8s.io/v1 CustomResourceDefinition, got %s", key, docIdx, gvk)
			}
			out = append(out, crd)
		}
	}
	return out, nil
}

// applyCRD performs a server-side apply of a single CRD.
//
// The three fields that webhook-injector owns on a Webhook conversion
// strategy — spec.conversion.webhook.clientConfig.{caBundle,url,service} —
// are intentionally stripped from the apply body whenever the CRD already
// exists in the target cluster, so webhook-injector remains their sole
// field-owner across re-applies. ForceOwnership is deliberately NOT used:
// without those fields in our body there is nothing to force, and keeping
// it on would let crd-injector steal future webhook-injector-owned fields.
//
// On first creation (CRD not yet in the target cluster) the user-authored
// clientConfig is left intact: the apiserver requires `url` or `service`
// to be set when strategy=Webhook, and webhook-injector's bootstrap
// admission MWC rewrites the entry during the same admission cycle.
// Admission-time mutations are attributed to the apiserver rather than to
// crd-injector, so our managed-fields entry still does not list those
// three keys after the create completes.
//
// We marshal to JSON and use RawPatch with types.ApplyPatchType rather than
// client.Apply: the latter is deprecated in controller-runtime v0.23 in favor
// of typed ApplyConfiguration objects, which would require pulling in
// apiextensions apply-config generators we don't otherwise need.
func applyCRD(ctx context.Context, c client.Client, crd *apiextensionsv1.CustomResourceDefinition) error {
	// Strip server-populated fields and ensure TypeMeta — server-side apply
	// requires apiVersion/kind in the body and rejects resourceVersion.
	crd.SetResourceVersion("")
	crd.SetUID("")
	crd.SetSelfLink("")
	crd.SetGeneration(0)
	crd.SetCreationTimestamp(metav1.Time{})
	crd.Status = apiextensionsv1.CustomResourceDefinitionStatus{}
	crd.APIVersion = apiextensionsv1.SchemeGroupVersion.String()
	crd.Kind = "CustomResourceDefinition"

	// If the CRD already exists, drop the webhook-injector-owned fields
	// from our apply body. With server-side apply, fields absent from the
	// body are not owned by the applier, so re-applies become a true no-op
	// for caBundle / url / service and we don't fight whatever
	// webhook-injector has stamped (rotated cert, Service→URL rewrite).
	existing := &apiextensionsv1.CustomResourceDefinition{}
	switch err := c.Get(ctx, types.NamespacedName{Name: crd.Name}, existing); {
	case err == nil:
		stripWebhookInjectorFields(crd)
	case apierrors.IsNotFound(err):
		// First install: keep the user-authored clientConfig so the
		// apiserver's CRD validator accepts the create.
	default:
		return fmt.Errorf("get existing: %w", err)
	}

	body, err := json.Marshal(crd)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	target := &apiextensionsv1.CustomResourceDefinition{}
	target.SetName(crd.Name)
	return c.Patch(ctx, target, client.RawPatch(types.ApplyPatchType, body), client.FieldOwner(fieldManager))
}

// stripWebhookInjectorFields zeroes the three fields under
// spec.conversion.webhook.clientConfig that webhook-injector owns at
// runtime — caBundle, url, and service. All three are omitempty in their
// API types, so the subsequent JSON marshal omits the keys entirely and
// crd-injector does not declare ownership of them via server-side apply.
//
// The surrounding clientConfig object is left in place: a CRD with
// strategy=Webhook is required by the apiserver to carry one, and removing
// the wrapper would change the structural shape of the apply body.
func stripWebhookInjectorFields(crd *apiextensionsv1.CustomResourceDefinition) {
	conv := crd.Spec.Conversion
	if conv == nil || conv.Strategy != apiextensionsv1.WebhookConverter {
		return
	}
	if conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		return
	}
	cc := conv.Webhook.ClientConfig
	cc.CABundle = nil
	cc.URL = nil
	cc.Service = nil
}

func detectNamespace() (string, error) {
	if ns := strings.TrimSpace(os.Getenv("NAMESPACE")); ns != "" {
		return ns, nil
	}
	const serviceAccountFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	data, err := os.ReadFile(serviceAccountFile)
	if err == nil && len(data) > 0 {
		return strings.TrimSpace(string(data)), nil
	}
	return "", errors.New("namespace not specified via --namespace, NAMESPACE env var unset, and serviceaccount namespace file not readable")
}

func buildHostingClient() (client.Client, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("hosting cluster config: %w", err)
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

func buildTargetClient() (client.Client, error) {
	cfg, err := targetRestConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

func targetRestConfig() (*rest.Config, error) {
	if targetKubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", targetKubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig %s: %w", targetKubeconfig, err)
		}
		return cfg, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return cfg, nil
}
