// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"bytes"
	"context"
	"fmt"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	resourcesv1alpha1 "github.com/gardener/gardener/pkg/apis/resources/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	sigsyaml "sigs.k8s.io/yaml"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/managedresource"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/metrics"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/webhookhost"
)

// ManagedResourceReconciler stamps the conversion webhook clientConfig of any
// CustomResourceDefinition embedded in a Gardener ManagedResource's referenced
// Secret on the *hosting* cluster.
//
// Why this exists: when webhook-injector runs on a Gardener seed, the CRDs it
// must stamp do not live directly in the shoot apiserver — they are wrapped
// inside ManagedResource objects on the seed, and gardener-resource-manager
// (GRM) reconciles them into the shoot. Stamping the shoot-side CRD directly
// would produce a thrash loop: GRM would overwrite our stamp from the
// ManagedResource Secret on its next sync. Stamping the *source* (this
// reconciler) makes the stamp survive GRM's next sync.
//
// Mechanism: for every labeled ManagedResource on the hosting cluster, walk
// spec.secretRefs[*], decode each data key (handling brotli ".br"
// compression), find CRDs with conversion.strategy=Webhook, stamp
// clientConfig.caBundle and — when ExternalHost is non-empty and the entry is
// Service-form — rewrite to URL-form using webhookhost.BuildExternalURL.
// Documents we don't touch survive byte-for-byte (no YAML round-trip), so
// user-authored formatting is preserved.
//
// Idempotency: a no-op reconcile must produce zero Secret updates, otherwise
// GRM re-applies the unchanged CRD to the shoot every cycle, burning
// resourceVersion and noisy events for no behavioural reason. The change
// detection here compares each doc's desired caBundle + URL against what's
// already present and short-circuits when nothing changed.
//
// Leader election: leader-only (matches WebhookReconciler).
type ManagedResourceReconciler struct {
	// HostingClient reads ManagedResources and the referenced resource
	// Secrets on the hosting cluster. Cached via the manager — fine for
	// everything except the cert Secret, see CertSecretReader.
	HostingClient client.Client

	// CertSecretReader reads the hosting-cluster cert Secret. In production
	// this is `mgr.GetAPIReader()` so the read bypasses the controller-
	// runtime informer cache and goes straight to the apiserver.
	//
	// Why a dedicated, uncached reader for this one Secret: WebhookReconciler
	// writes a fresh cert Secret and immediately signals a rotation, which
	// fans out as GenericEvents to this reconciler. The triggered reconcile
	// almost always runs before the shared informer cache has absorbed the
	// new write — a cached Get would return the stale tls.crt, the
	// MR-embedded CRD's caBundle would compare equal, and the re-stamp
	// would silently no-op until the cache catches up (~minutes) or the
	// next `--sync-period` tick. The APIReader path removes the race for
	// the cost of one extra Get per reconcile pass. Every other read
	// (ManagedResources, MR-referenced Secrets) stays on the cached
	// HostingClient.
	//
	// Nil falls back to HostingClient so test code can keep using a single
	// fake client; production code in main.go always sets this.
	CertSecretReader client.Reader

	// Namespace / CertSecretName locate the hosting-cluster Secret holding
	// the freshly minted tls.crt that becomes the embedded CRDs' caBundle.
	Namespace      string
	CertSecretName string

	// WebhookLabelKey/Value scope reconciliation: only ManagedResources
	// carrying this label are watched and reconciled.
	WebhookLabelKey   string
	WebhookLabelValue string

	// ExternalHost, when non-empty, rewrites Service-form conversion
	// clientConfig entries to URL-form: clientConfig.URL =
	// "https://<ExternalHost><Service.Path>" (Service.Path nil/empty → "/"),
	// then clears clientConfig.Service. URL-form entries are left untouched.
	// Empty disables rewriting (caBundle still gets stamped).
	ExternalHost string

	// ForceReconcile, when true, patches the ManagedResource with the
	// `gardener.cloud/operation=reconcile` annotation after a successful
	// Secret write. This kicks gardener-resource-manager out of any backoff
	// or debounce state it might be in, ensuring our stamped Secret reaches
	// the shoot promptly. The annotation is set only when at least one
	// Secret was actually written this reconcile — never on the
	// idempotent no-op path — so no-change reconciles do not churn GRM.
	//
	// GRM removes the annotation itself after consuming it (well-defined
	// Gardener behaviour), so we don't need to clean it up.
	ForceReconcile bool

	// Gate publishes the set of CRDs this reconciler is responsible for,
	// keyed by ManagedResource. WebhookReconciler reads the union of all
	// MRs' contributions during the staged rotation phase to decide
	// which target-cluster CRDs to poll for caBundle propagation before
	// promoting the new serving cert.
	//
	// Nil disables publishing (the gate's snapshot stays empty for this
	// reconciler) — fine for tests that don't exercise rotation, and a
	// no-op for production deployments without a WebhookReconciler
	// wired to the same Gate.
	Gate *rotationgate.Gate
}

// Gardener annotation contract for "kick this object to reconcile". The
// upstream constants live in github.com/gardener/gardener/pkg/apis/core/v1beta1/constants
// (GardenerOperation / GardenerOperationReconcile), but importing that
// sub-package would pull in the entire core API for two string literals. The
// values themselves are a stable public contract — Gardener documents them
// as the user-facing way to trigger reconcile — so we declare them locally.
const (
	gardenerOperationAnnotation = "gardener.cloud/operation"
	gardenerOperationReconcile  = "reconcile"
)

// SetupWithManager registers a leader-only controller that watches labeled
// ManagedResource objects on the hosting cluster and reconciles them. It also
// registers a manager.Runnable that fans out rotationTrigger receives into
// per-MR Reconcile.Requests so a fresh caBundle propagates immediately rather
// than waiting for the next --sync-period.
//
// A nil rotationTrigger disables the fan-out (the runnable still registers
// but blocks forever) for tests that don't exercise rotation signalling.
func (r *ManagedResourceReconciler) SetupWithManager(mgr ctrl.Manager, rotationTrigger <-chan struct{}) error {
	mrCh := make(chan event.GenericEvent, 64)

	resync := &managedResourceResyncTrigger{
		hostingClient:     r.HostingClient,
		webhookLabelKey:   r.WebhookLabelKey,
		webhookLabelValue: r.WebhookLabelValue,
		rotationTrigger:   rotationTrigger,
		mrCh:              mrCh,
	}
	if err := mgr.Add(resync); err != nil {
		return fmt.Errorf("failed to register managed-resource resync trigger runnable: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("managed-resource").
		For(&resourcesv1alpha1.ManagedResource{}, builder.WithPredicates(r.labelPredicate())).
		WatchesRawSource(source.Channel(
			mrCh,
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
				return []ctrl.Request{{NamespacedName: types.NamespacedName{
					Name:      obj.GetName(),
					Namespace: obj.GetNamespace(),
				}}}
			}),
		)).
		Complete(reconcile.Func(r.Reconcile))
}

// labelPredicate filters ManagedResource events down to those carrying the
// configured webhook label. Belt-and-suspenders against the hosting-cluster
// cache also being label-scoped in main.go.
func (r *ManagedResourceReconciler) labelPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		if obj == nil {
			return false
		}
		return obj.GetLabels()[r.WebhookLabelKey] == r.WebhookLabelValue
	})
}

// Reconcile stamps every CRD embedded in the ManagedResource's referenced
// Secrets with the current caBundle (and optionally rewrites Service-form
// conversion webhooks to URL-form). Short-circuits when nothing has
// changed — both at the document level (skip docs already converged) and at
// the Secret level (skip the Update when no document in this Secret was
// touched).
func (r *ManagedResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", "ManagedResource", "name", req.NamespacedName)
	result := metrics.ResultConverged
	defer func() { metrics.ManagedResourceStampsTotal.WithLabelValues(result).Inc() }()

	// gateKey is the per-MR identity used for rotation-gate publishing.
	// We compute it once up front so Forget and Publish below share the
	// same shape.
	gateKey := rotationgate.Key(req.String())

	mr := &resourcesv1alpha1.ManagedResource{}
	if err := r.HostingClient.Get(ctx, req.NamespacedName, mr); err != nil {
		if apierrors.IsNotFound(err) {
			// MR was deleted (or never existed). Drop its contribution to
			// the rotation gate so a future rotation doesn't wait on a
			// CRD that's no longer under our control.
			if r.Gate != nil {
				r.Gate.Forget(gateKey)
			}
			return ctrl.Result{}, nil
		}
		result = metrics.ResultError
		return ctrl.Result{}, fmt.Errorf("failed to get ManagedResource %s: %w", req.NamespacedName, err)
	}

	// Re-check the label here — the cache predicate gates *events*, but a
	// delete-then-recreate without the label could still leak a stale entry
	// through if we trusted the predicate alone.
	if mr.Labels[r.WebhookLabelKey] != r.WebhookLabelValue {
		// MR left our label scope. Same treatment as deletion for the gate:
		// stop publishing its CRDs so the gate doesn't wait on them.
		if r.Gate != nil {
			r.Gate.Forget(gateKey)
		}
		return ctrl.Result{}, nil
	}

	// Surface GRM health for the MR before we do anything else. We do not
	// gate the stamp on health — an unhealthy MR still needs the fresh
	// caBundle, and refusing to update it makes the unhealthiness worse —
	// but we want every reconcile pass (including periodic --sync-period
	// ticks) to emit a fresh observation so dashboards reflect current
	// state. Missing conditions (freshly created MR before GRM has
	// evaluated) are deliberately ignored to avoid noise.
	r.observeConditionHealth(mr, logger)

	caCert, err := r.loadCABundle(ctx)
	if err != nil {
		result = metrics.ResultError
		return ctrl.Result{}, err
	}

	totalStamped := 0
	totalObserved := 0
	var publishedCRDs []string
	for _, ref := range mr.Spec.SecretRefs {
		stamped, observed, crds, err := r.stampSecret(ctx, mr.Namespace, ref.Name, caCert, logger)
		if err != nil {
			result = metrics.ResultError
			return ctrl.Result{}, err
		}
		totalStamped += stamped
		totalObserved += observed
		publishedCRDs = append(publishedCRDs, crds...)
	}

	// Publish the gate set for this MR on every reconcile (including
	// idempotent no-ops): even when the Secret didn't change, the set of
	// CRDs the controller is responsible for stays stable, and the
	// rotation gate should reflect that. Gate.Publish canonicalises the
	// input, so duplicates across secretRefs are handled.
	if r.Gate != nil {
		r.Gate.Publish(gateKey, publishedCRDs)
	}

	// Enforce the spec.keepObjects=true invariant on MRs that ship CRDs:
	// without it, deleting the ManagedResource would cascade-delete the
	// shoot's CRDs, taking every custom resource with them. We do not block
	// the reconcile when the flag is missing — the CRD still needs the
	// fresh caBundle and we'd rather have a working webhook than refuse to
	// stamp — but we surface the misconfiguration as a warning-level log
	// and a counter so an operator dashboard catches it. Emitted via
	// logger.Info (not Error) so it does not carry a stack trace; the
	// "WARNING:" prefix keeps it visually distinct from informational lines.
	if totalObserved > 0 && !keepObjectsTrue(mr) {
		metrics.ManagedResourceKeepObjectsViolationsTotal.Inc()
		logger.Info("WARNING: ManagedResource ships CRDs but spec.keepObjects is not true — deleting this MR would cascade-delete the shoot CRDs and every custom resource of those kinds. Set spec.keepObjects: true on the ManagedResource.",
			"crdsObserved", totalObserved,
		)
	}

	if totalStamped > 0 {
		result = metrics.ResultStamped
		metrics.ManagedResourceCRDsStampedTotal.Add(float64(totalStamped))
		logger.Info("Stamped ManagedResource", "secretRefs", len(mr.Spec.SecretRefs), "crdsStamped", totalStamped)
		if r.ForceReconcile {
			if err := r.patchForceReconcile(ctx, mr); err != nil {
				// Don't fail the reconcile — the Secret update has already
				// landed and GRM's own Secret watch will pick it up on its
				// next reconcile. The annotation is belt-and-suspenders for
				// cases where GRM is in backoff. Log and move on so a flaky
				// annotation patch doesn't mask a successful stamp.
				logger.Error(err, "Failed to patch force-reconcile annotation; relying on GRM's Secret watch")
			}
		}
	}
	return ctrl.Result{}, nil
}

// keepObjectsTrue reports whether the MR's spec.keepObjects is explicitly
// set to true. The field is *bool — nil and false both fail this check,
// because Gardener's default when unset is false (objects are
// cascade-deleted with the MR).
func keepObjectsTrue(mr *resourcesv1alpha1.ManagedResource) bool {
	return mr.Spec.KeepObjects != nil && *mr.Spec.KeepObjects
}

// MR condition types we surface health for. Values match the constants in
// resourcesv1alpha1 (ResourcesApplied / ResourcesHealthy); declared locally
// as plain strings so we can also use them as metric label values without
// casting from the gardener ConditionType type alias.
const (
	conditionResourcesApplied = "ResourcesApplied"
	conditionResourcesHealthy = "ResourcesHealthy"
)

// trackedConditions lists the MR condition types whose health we surface.
var trackedConditions = []string{conditionResourcesApplied, conditionResourcesHealthy}

// observeConditionHealth inspects mr.Status.Conditions for ResourcesApplied
// and ResourcesHealthy. For each condition that is present but not
// Status=True it logs a warning (Info level with a "WARNING:" prefix — not
// Error, so no stack trace) and increments
// ManagedResourceUnhealthyTotal{condition,status}. The counter is the
// alertable signal; the log is its human-readable companion.
//
// Missing conditions are intentionally ignored: a freshly created MR may
// not have status populated yet, and counting that as unhealthy would
// generate noise during normal MR creation. GRM populates conditions
// within seconds, and subsequent reconciles will then observe them.
//
// This is purely observational — the reconcile is never aborted on an
// unhealthy condition. Status=Unknown with reason=ChecksPending is a
// normal transient state right after GRM applies resources, and even a
// chronic Status=False is something we want to surface, not act on.
func (r *ManagedResourceReconciler) observeConditionHealth(mr *resourcesv1alpha1.ManagedResource, logger logr.Logger) {
	for _, wantType := range trackedConditions {
		cond := findCondition(mr.Status.Conditions, wantType)
		if cond == nil {
			// Not yet evaluated by GRM — skip, see comment above.
			continue
		}
		if cond.Status == "True" {
			continue
		}
		metrics.ManagedResourceUnhealthyTotal.WithLabelValues(wantType, string(cond.Status)).Inc()
		logger.Info("WARNING: ManagedResource condition is not True",
			"condition", wantType,
			"status", cond.Status,
			"reason", cond.Reason,
			"message", cond.Message,
		)
	}
}

// findCondition returns a pointer to the condition with the given type, or
// nil if not present. The Gardener Condition type is referenced via the
// surrounding []gardencorev1beta1.Condition slice without an explicit
// import: we only read the public string fields, and the slice element
// type comes through transitively from resourcesv1alpha1.ManagedResource.
func findCondition(conds []gardencorev1beta1.Condition, condType string) *gardencorev1beta1.Condition {
	for i := range conds {
		if string(conds[i].Type) == condType {
			return &conds[i]
		}
	}
	return nil
}

// stampSecret reads one referenced Secret, mutates any embedded CRDs, and
// writes the Secret back only when at least one CRD doc changed. Returns the
// number of CRD docs that were rewritten in this Secret (stamped) and the
// number of CRD docs observed regardless of stamping (observed). The
// observed count is what drives the spec.keepObjects=true check in the
// caller — a CRD with conversion.strategy=None still counts as a CRD that
// the MR is shipping into the shoot, even though we don't rewrite it.
func (r *ManagedResourceReconciler) stampSecret(ctx context.Context, namespace, name string, caCert []byte, logger logr.Logger) (stamped, observed int, crdNames []string, err error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: name, Namespace: namespace}
	if err := r.HostingClient.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Reference dangling — log and skip. Gardener will eventually
			// either create the Secret (and we get a fresh MR event) or
			// remove the ref.
			logger.V(1).Info("Referenced Secret not found, skipping", "secret", key)
			return 0, 0, nil, nil
		}
		return 0, 0, nil, fmt.Errorf("failed to get Secret %s: %w", key, err)
	}

	stampedTotal := 0
	observedTotal := 0
	secretChanged := false

	// Iterate over data keys in a stable way is not required for correctness
	// — keys are independent — but iterating directly over the map is fine.
	for dataKey, raw := range secret.Data {
		compressedKey := managedresource.IsCompressedKey(dataKey)
		if compressedKey {
			metrics.ManagedResourceSecretKeysTotal.WithLabelValues(metrics.EncodingBrotli).Inc()
		} else {
			metrics.ManagedResourceSecretKeysTotal.WithLabelValues(metrics.EncodingPlain).Inc()
		}
		plain, compressed, err := managedresource.Decode(compressedKey, raw)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("decode %s/%s[%s]: %w", namespace, name, dataKey, err)
		}

		docs, err := managedresource.SplitDocs(plain)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("split %s/%s[%s]: %w", namespace, name, dataKey, err)
		}

		keyChanged := false
		for i, doc := range docs {
			rewritten, changed, wasCRD, crdName, err := r.maybeStampCRDDoc(doc, caCert)
			if err != nil {
				return 0, 0, nil, fmt.Errorf("stamp %s/%s[%s] doc#%d: %w", namespace, name, dataKey, i, err)
			}
			if wasCRD {
				observedTotal++
			}
			if crdName != "" {
				// We publish every CRD we're responsible for, whether or
				// not we wrote it this pass. That gives the rotation gate
				// a stable view across idempotent reconciles.
				crdNames = append(crdNames, crdName)
			}
			if changed {
				docs[i] = rewritten
				keyChanged = true
				stampedTotal++
			}
		}
		if !keyChanged {
			continue
		}

		newPlain := managedresource.JoinDocs(docs)
		newRaw, err := managedresource.Encode(newPlain, compressed)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("encode %s/%s[%s]: %w", namespace, name, dataKey, err)
		}
		secret.Data[dataKey] = newRaw
		secretChanged = true
	}

	if !secretChanged {
		return 0, observedTotal, crdNames, nil
	}

	if err := r.HostingClient.Update(ctx, secret); err != nil {
		return 0, observedTotal, crdNames, fmt.Errorf("failed to update Secret %s: %w", key, err)
	}
	logger.Info("Updated ManagedResource resource Secret", "secret", key, "crdsStamped", stampedTotal)
	return stampedTotal, observedTotal, crdNames, nil
}

// crdKind is the Kind name we match when scanning multi-doc YAML for CRDs.
// Extracted so both the production gate (crdGVK) and the test fixtures use
// the same string — and so goconst stops counting occurrences.
const crdKind = "CustomResourceDefinition"

// kindManagedResource is the metrics-label kind value for Gardener
// ManagedResource objects watched on the hosting cluster. Kept alongside
// the other kind constants (kindMWC/kindVWC/crdKind) so a typo can't
// silently create a second time series.
const kindManagedResource = "ManagedResource"

// crdGVK is the GroupVersionKind we recognise as a CRD doc. We deliberately
// match the v1 group only — apiextensions.k8s.io/v1beta1 has been deprecated
// for years and Gardener-managed CRDs are all v1.
var crdGVK = schema.GroupVersionKind{
	Group:   apiextensionsv1.GroupName,
	Version: "v1",
	Kind:    crdKind,
}

// maybeStampCRDDoc inspects a single YAML document. If it is not a CRD with a
// Webhook conversion strategy, the input is returned unchanged with
// changed=false. Otherwise it stamps caBundle and (optionally) rewrites the
// Service-form clientConfig to URL-form. The returned bytes are a freshly
// marshalled YAML representation — only stamped docs go through this
// serializer, so unstamped docs in the same Secret survive byte-for-byte.
//
// `observed` reports whether the doc was identified as an
// apiextensions.k8s.io/v1 CustomResourceDefinition regardless of whether it
// was actually stamped. The caller uses this to enforce the
// `spec.keepObjects=true` invariant on MRs that ship CRDs — a CRD with
// `conversion.strategy=None` still counts as a CRD for that purpose, so
// `observed` is set independently of `changed`.
//
// `crdName` is the apiextensions metadata.name of the observed CRD, or
// the empty string when the doc isn't a CRD. The caller publishes this
// name on the shared rotation-gate snapshot regardless of whether we
// rewrote the doc, because gating is about which CRDs the controller
// is *responsible* for — not which it has just changed.
func (r *ManagedResourceReconciler) maybeStampCRDDoc(doc, caCert []byte) (out []byte, changed, observed bool, crdName string, err error) {
	// Cheap GVK gate: parse just TypeMeta first so we don't pay the full
	// CRD JSON deserializer cost on every ConfigMap / Deployment / etc. in
	// the same Secret.
	var typeOnly metav1.TypeMeta
	if err := sigsyaml.Unmarshal(doc, &typeOnly); err != nil {
		// Non-YAML or malformed input — leave the doc untouched. We'd rather
		// pass through something we don't understand than fail a rotation
		// because GRM packed an unexpected blob in the Secret. Returning a
		// nil error here is the deliberate contract of maybeStampCRDDoc:
		// errors are reported only for docs we recognised as CRDs and
		// failed to handle; an unparsable non-CRD doc is the non-CRD
		// case, not an error case.
		return doc, false, false, "", nil //nolint:nilerr // intentional: see comment
	}
	if typeOnly.APIVersion != crdGVK.GroupVersion().String() || typeOnly.Kind != crdGVK.Kind {
		return doc, false, false, "", nil
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := sigsyaml.Unmarshal(doc, crd); err != nil {
		return nil, false, true, "", fmt.Errorf("decode CRD: %w", err)
	}

	conv := crd.Spec.Conversion
	if conv == nil || conv.Strategy != apiextensionsv1.WebhookConverter || conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		// CRD with strategy=None or a missing clientConfig — nothing to
		// stamp. Still counts as observed because the MR ships a CRD.
		// We do NOT publish this CRD's name on the rotation gate: gating
		// only matters for CRDs whose caBundle we control.
		return doc, false, true, "", nil
	}

	cc := conv.Webhook.ClientConfig
	caChanged := !bytes.Equal(cc.CABundle, caCert)
	urlChanged := false
	if r.ExternalHost != "" && cc.Service != nil {
		url := webhookhost.BuildExternalURL(r.ExternalHost, cc.Service.Path)
		cc.URL = &url
		cc.Service = nil
		urlChanged = true
	}
	if !caChanged && !urlChanged {
		// Already converged — preserve the original bytes so we don't
		// trigger a Secret update for this doc. We still return the CRD
		// name so the caller can publish it on the gate (we're
		// responsible for keeping it stamped on rotation).
		return doc, false, true, crd.Name, nil
	}
	cc.CABundle = caCert

	rewritten, err := sigsyaml.Marshal(crd)
	if err != nil {
		return nil, false, true, crd.Name, fmt.Errorf("encode CRD: %w", err)
	}
	return rewritten, true, true, crd.Name, nil
}

// loadCABundle reads tls.crt from the hosting-cluster certificate Secret via
// the shared package-private helper. Routes through CertSecretReader when
// set (typically mgr.GetAPIReader() in production, dodging the rotation
// propagation race documented on the field), and falls back to
// HostingClient when nil so test code can keep using a single fake client.
func (r *ManagedResourceReconciler) loadCABundle(ctx context.Context) ([]byte, error) {
	reader := client.Reader(r.HostingClient)
	if r.CertSecretReader != nil {
		reader = r.CertSecretReader
	}
	return loadCABundle(ctx, reader, r.Namespace, r.CertSecretName)
}

// patchForceReconcile stamps `gardener.cloud/operation=reconcile` on the MR
// via a merge-from patch so we only touch one annotation. If the annotation
// is already present (e.g. from a previous reconcile GRM has not yet consumed)
// we skip the API call — re-setting the same value is a no-op but still
// counts as an MR write, which we don't need.
//
// We deliberately do NOT clear the annotation: GRM removes it itself after
// consuming, which is the documented Gardener contract.
func (r *ManagedResourceReconciler) patchForceReconcile(ctx context.Context, mr *resourcesv1alpha1.ManagedResource) error {
	if mr.Annotations[gardenerOperationAnnotation] == gardenerOperationReconcile {
		metrics.ManagedResourceForceReconcileTotal.WithLabelValues(metrics.ForceReconcileSkipped).Inc()
		return nil
	}
	base := mr.DeepCopy()
	if mr.Annotations == nil {
		mr.Annotations = map[string]string{}
	}
	mr.Annotations[gardenerOperationAnnotation] = gardenerOperationReconcile
	if err := r.HostingClient.Patch(ctx, mr, client.MergeFrom(base)); err != nil {
		metrics.ManagedResourceForceReconcileTotal.WithLabelValues(metrics.ForceReconcileError).Inc()
		return fmt.Errorf("patch %s/%s with %s=%s: %w",
			mr.Namespace, mr.Name, gardenerOperationAnnotation, gardenerOperationReconcile, err)
	}
	metrics.ManagedResourceForceReconcileTotal.WithLabelValues(metrics.ForceReconcilePatched).Inc()
	return nil
}

// managedResourceResyncTrigger is a manager.Runnable that fans out a rotation
// signal into the ManagedResourceReconciler's GenericEvent channel. Each
// receive on rotationTrigger lists every labeled ManagedResource currently
// in the hosting cluster cache and emits one GenericEvent per object so the
// controller enqueues a Reconcile.Request.
//
// Runs on the leader only (matches the controller it feeds). A nil
// rotationTrigger leaves the runnable idle — select blocks forever on a nil
// channel, which is exactly the no-op behaviour we want when the caller has
// not wired up rotation signalling.
type managedResourceResyncTrigger struct {
	hostingClient     client.Client
	webhookLabelKey   string
	webhookLabelValue string
	rotationTrigger   <-chan struct{}
	mrCh              chan event.GenericEvent
}

// Start blocks until ctx is cancelled, fanning out one resync per
// rotationTrigger receive. Errors during list (e.g., cache not yet synced)
// are logged and skipped — the next rotation will retry, and the periodic
// --sync-period reconcile is the ultimate safety net.
func (t *managedResourceResyncTrigger) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("managed-resource-resync-trigger")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.rotationTrigger:
			t.fanOut(ctx, logger)
		}
	}
}

// fanOut lists every labeled ManagedResource and pushes one GenericEvent
// per object into mrCh. The send blocks until the consumer drains a slot
// (or the context is cancelled), so the producer is naturally paced by
// the reconcile rate. Earlier versions of this code dropped events when
// the buffer was full; that lost rotation propagation to some MRs when
// >64 labeled MRs existed or the consumer was briefly slow, leaving
// shoot-side CRDs trusting the old cert until --sync-period reconverged.
func (t *managedResourceResyncTrigger) fanOut(ctx context.Context, logger logr.Logger) {
	selector := client.MatchingLabels{t.webhookLabelKey: t.webhookLabelValue}

	var list resourcesv1alpha1.ManagedResourceList
	if err := t.hostingClient.List(ctx, &list, selector); err != nil {
		logger.Error(err, "rotation fan-out: failed to list ManagedResources")
		return
	}
	for i := range list.Items {
		obj := &list.Items[i]
		select {
		case t.mrCh <- event.GenericEvent{Object: obj}:
			metrics.RotationFanOutEventsTotal.WithLabelValues(kindManagedResource, metrics.FanOutEmitted).Inc()
		case <-ctx.Done():
			return
		}
	}
	logger.V(1).Info("rotation fan-out complete", "managedResources", len(list.Items))
}

// NeedLeaderElection ensures the fan-out only fires on the leader, matching
// the leader-only controller it feeds.
func (t *managedResourceResyncTrigger) NeedLeaderElection() bool { return true }

// Compile-time assertions that managedResourceResyncTrigger satisfies the
// manager interfaces it needs.
var (
	_ manager.Runnable               = (*managedResourceResyncTrigger)(nil)
	_ manager.LeaderElectionRunnable = (*managedResourceResyncTrigger)(nil)
)
