// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/webhookhost"
)

// TargetWebhookReconciler stamps caBundle (and optionally rewrites Service-form
// clientConfig to URL-form) on labeled MutatingWebhookConfiguration,
// ValidatingWebhookConfiguration, and CustomResourceDefinition resources living
// in the *target* cluster.
//
// MWC/VWC: kube-apiserver self-protects against admission webhooks on
// admissionregistration.k8s.io/{mutating,validating}webhookconfigurations
// (rules.IsExemptAdmissionConfigurationResource), so the bootstrap MWC cannot
// catch CREATE/UPDATE on those kinds. A target-cluster Watch is the only way
// to converge on changes.
//
// CRDs: the bootstrap MWC *can* register on CRDs, so an admission hook handles
// CREATE/UPDATE synchronously. The Watch reconciler here is the drift-correction
// path — out-of-band edits to existing CRDs (manual kubectl, GitOps overwrites,
// other controllers stamping a stale caBundle) converge in seconds instead of
// being bounded by --sync-period.
type TargetWebhookReconciler struct {
	// HostingClient reads the certificate Secret from the hosting cluster.
	HostingClient client.Client
	// TargetClient updates labeled MWC/VWC objects in the target cluster.
	// Sourced from the target cluster.Cluster handle in main.go.
	TargetClient client.Client

	// Namespace and CertSecretName locate the hosting-cluster Secret holding
	// the freshly minted tls.crt that becomes the target objects' caBundle.
	Namespace      string
	CertSecretName string

	// WebhookLabelKey/Value scope reconciliation: only labeled objects are
	// watched (via a label-selector predicate) and reconciled. Belt-and-
	// suspenders against the cache also being label-scoped in main.go.
	WebhookLabelKey   string
	WebhookLabelValue string

	// ExternalHost, when non-empty, rewrites Service-form clientConfig entries
	// to URL-form: clientConfig.URL = "https://<ExternalHost><Service.Path>"
	// (Service.Path nil/empty → "/"), then clears clientConfig.Service.
	// URL-form entries are left untouched. Empty disables rewriting.
	ExternalHost string
}

// SetupWithManager registers three leader-only controllers on mgr that watch
// MutatingWebhookConfiguration, ValidatingWebhookConfiguration, and
// CustomResourceDefinition resources on the *target* cluster's cache (not the
// manager's own hosting-cluster cache).
//
// Each controller filters incoming events through a label-selector predicate
// so non-managed objects do not enqueue work. The reconcile body refetches
// the object via TargetClient (live read against the target cluster) to
// reduce cache-staleness write conflicts.
//
// rotationTrigger, when non-nil, is the read end of a channel signalled by
// WebhookReconciler whenever a fresh certificate has been written to the
// hosting-cluster Secret. Each receive on that channel triggers a fan-out:
// every labeled MWC/VWC/CRD currently visible in the target cluster's cache
// is enqueued for reconcile so its caBundle converges immediately rather
// than waiting for the next --sync-period tick. A nil rotationTrigger
// disables the fan-out (the runnable still registers but blocks forever),
// which is convenient for tests that don't exercise rotation signalling.
func (r *TargetWebhookReconciler) SetupWithManager(mgr ctrl.Manager, targetCluster cluster.Cluster, rotationTrigger <-chan struct{}) error {
	// Per-kind buffered channels feeding source.Channel for each of the three
	// controllers. Buffer size 64 absorbs a typical fleet (we drop on full
	// rather than block the trigger — see targetResyncTrigger.Start).
	//
	// These channels are always non-nil even when rotationTrigger is nil:
	// source.Channel requires a non-nil channel, and an idle channel is
	// harmless (no events ever flow through it).
	mwcCh := make(chan event.GenericEvent, 64)
	vwcCh := make(chan event.GenericEvent, 64)
	crdCh := make(chan event.GenericEvent, 64)

	resync := &targetResyncTrigger{
		targetClient:      targetCluster.GetClient(),
		webhookLabelKey:   r.WebhookLabelKey,
		webhookLabelValue: r.WebhookLabelValue,
		rotationTrigger:   rotationTrigger,
		mwcCh:             mwcCh,
		vwcCh:             vwcCh,
		crdCh:             crdCh,
	}
	if err := mgr.Add(resync); err != nil {
		return fmt.Errorf("failed to register target-resync trigger runnable: %w", err)
	}

	// MutatingWebhookConfiguration watcher.
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("target-mwc").
		WatchesRawSource(source.Kind(
			targetCluster.GetCache(),
			&admissionv1.MutatingWebhookConfiguration{},
			&handler.TypedEnqueueRequestForObject[*admissionv1.MutatingWebhookConfiguration]{},
			r.labelPredicateMWC(),
		)).
		WatchesRawSource(source.Channel(
			mwcCh,
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
				return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: obj.GetName()}}}
			}),
		)).
		Complete(reconcile.Func(r.reconcileMWC)); err != nil {
		return fmt.Errorf("failed to set up target-mwc controller: %w", err)
	}

	// ValidatingWebhookConfiguration watcher.
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("target-vwc").
		WatchesRawSource(source.Kind(
			targetCluster.GetCache(),
			&admissionv1.ValidatingWebhookConfiguration{},
			&handler.TypedEnqueueRequestForObject[*admissionv1.ValidatingWebhookConfiguration]{},
			r.labelPredicateVWC(),
		)).
		WatchesRawSource(source.Channel(
			vwcCh,
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
				return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: obj.GetName()}}}
			}),
		)).
		Complete(reconcile.Func(r.reconcileVWC)); err != nil {
		return fmt.Errorf("failed to set up target-vwc controller: %w", err)
	}

	// CustomResourceDefinition watcher. The admission webhook bootstrapped by
	// WebhookReconciler stamps CRDs synchronously on CREATE/UPDATE — this
	// Watch is the drift-correction path for out-of-band edits.
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("target-crd").
		WatchesRawSource(source.Kind(
			targetCluster.GetCache(),
			&apiextensionsv1.CustomResourceDefinition{},
			&handler.TypedEnqueueRequestForObject[*apiextensionsv1.CustomResourceDefinition]{},
			r.labelPredicateCRD(),
		)).
		WatchesRawSource(source.Channel(
			crdCh,
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
				return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: obj.GetName()}}}
			}),
		)).
		Complete(reconcile.Func(r.reconcileCRD)); err != nil {
		return fmt.Errorf("failed to set up target-crd controller: %w", err)
	}

	return nil
}

// targetResyncTrigger is a manager.Runnable that fans out a rotation signal
// into per-kind GenericEvent channels feeding the three target-cluster
// controllers. Each receive on rotationTrigger lists every labeled MWC, VWC,
// and CRD currently in the target cluster cache and emits one GenericEvent
// per object so the corresponding controller enqueues a Reconcile.Request.
//
// Runs on the leader (NeedLeaderElection returns true), matching the leader-
// only controllers it feeds. A nil rotationTrigger leaves the runnable idle —
// the select blocks forever on a nil channel, which is exactly the no-op
// behaviour we want when the caller has not wired up rotation signalling.
type targetResyncTrigger struct {
	targetClient      client.Client
	webhookLabelKey   string
	webhookLabelValue string
	rotationTrigger   <-chan struct{}
	mwcCh             chan event.GenericEvent
	vwcCh             chan event.GenericEvent
	crdCh             chan event.GenericEvent
}

// Start blocks until ctx is cancelled, fanning out one resync per
// rotationTrigger receive. Errors during list (e.g., cache not yet synced)
// are logged and skipped — the next rotation will retry, and the periodic
// --sync-period reconcile is the ultimate safety net.
func (t *targetResyncTrigger) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("target-resync-trigger")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.rotationTrigger:
			t.fanOut(ctx, logger)
		}
	}
}

// fanOut lists every labeled MWC/VWC/CRD in the target cluster and pushes
// one GenericEvent per object into the corresponding per-kind channel. A
// full channel means a previous rotation is still draining; we log and
// drop rather than block, since the next rotation (or --sync-period) will
// reconverge.
func (t *targetResyncTrigger) fanOut(ctx context.Context, logger logr.Logger) {
	selector := client.MatchingLabels{t.webhookLabelKey: t.webhookLabelValue}

	var mwcList admissionv1.MutatingWebhookConfigurationList
	if err := t.targetClient.List(ctx, &mwcList, selector); err != nil {
		logger.Error(err, "rotation fan-out: failed to list MutatingWebhookConfigurations")
	} else {
		for i := range mwcList.Items {
			obj := &mwcList.Items[i]
			select {
			case t.mwcCh <- event.GenericEvent{Object: obj}:
			default:
				logger.Info("rotation fan-out: MWC channel full, dropping event", "name", obj.Name)
			}
		}
	}

	var vwcList admissionv1.ValidatingWebhookConfigurationList
	if err := t.targetClient.List(ctx, &vwcList, selector); err != nil {
		logger.Error(err, "rotation fan-out: failed to list ValidatingWebhookConfigurations")
	} else {
		for i := range vwcList.Items {
			obj := &vwcList.Items[i]
			select {
			case t.vwcCh <- event.GenericEvent{Object: obj}:
			default:
				logger.Info("rotation fan-out: VWC channel full, dropping event", "name", obj.Name)
			}
		}
	}

	var crdList apiextensionsv1.CustomResourceDefinitionList
	if err := t.targetClient.List(ctx, &crdList, selector); err != nil {
		logger.Error(err, "rotation fan-out: failed to list CustomResourceDefinitions")
	} else {
		for i := range crdList.Items {
			obj := &crdList.Items[i]
			select {
			case t.crdCh <- event.GenericEvent{Object: obj}:
			default:
				logger.Info("rotation fan-out: CRD channel full, dropping event", "name", obj.Name)
			}
		}
	}

	logger.V(1).Info("rotation fan-out complete",
		"mwcs", len(mwcList.Items),
		"vwcs", len(vwcList.Items),
		"crds", len(crdList.Items),
	)
}

// NeedLeaderElection ensures the fan-out only fires on the leader, matching
// the leader-only controllers it feeds. Without this, follower replicas
// would push events into channels whose controllers are not running.
func (t *targetResyncTrigger) NeedLeaderElection() bool { return true }

// Compile-time assertions that targetResyncTrigger satisfies the manager
// interfaces it needs.
var (
	_ manager.Runnable               = (*targetResyncTrigger)(nil)
	_ manager.LeaderElectionRunnable = (*targetResyncTrigger)(nil)
)

// labelPredicateMWC returns a typed predicate matching MWC objects whose labels
// include WebhookLabelKey=WebhookLabelValue. Belt-and-suspenders against the
// target-cluster cache also being label-scoped in main.go.
func (r *TargetWebhookReconciler) labelPredicateMWC() predicate.TypedPredicate[*admissionv1.MutatingWebhookConfiguration] {
	return predicate.NewTypedPredicateFuncs(func(obj *admissionv1.MutatingWebhookConfiguration) bool {
		if obj == nil {
			return false
		}
		return obj.Labels[r.WebhookLabelKey] == r.WebhookLabelValue
	})
}

// labelPredicateVWC mirrors labelPredicateMWC for ValidatingWebhookConfiguration.
func (r *TargetWebhookReconciler) labelPredicateVWC() predicate.TypedPredicate[*admissionv1.ValidatingWebhookConfiguration] {
	return predicate.NewTypedPredicateFuncs(func(obj *admissionv1.ValidatingWebhookConfiguration) bool {
		if obj == nil {
			return false
		}
		return obj.Labels[r.WebhookLabelKey] == r.WebhookLabelValue
	})
}

// labelPredicateCRD mirrors labelPredicateMWC for CustomResourceDefinition.
func (r *TargetWebhookReconciler) labelPredicateCRD() predicate.TypedPredicate[*apiextensionsv1.CustomResourceDefinition] {
	return predicate.NewTypedPredicateFuncs(func(obj *apiextensionsv1.CustomResourceDefinition) bool {
		if obj == nil {
			return false
		}
		return obj.Labels[r.WebhookLabelKey] == r.WebhookLabelValue
	})
}

// reconcileMWC stamps caBundle and (optionally) rewrites Service-form
// clientConfig on a single MutatingWebhookConfiguration in the target cluster.
// Short-circuits the Update when every entry's caBundle already matches and
// no Service→URL rewrite is needed — without this, every Update bumps
// resourceVersion, fires another Watch event, and the reconcile loops on
// itself. (CRDs have the same protection in reconcileCRD.)
func (r *TargetWebhookReconciler) reconcileMWC(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", "MutatingWebhookConfiguration", "name", req.Name)

	caCert, err := r.loadCABundle(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	mwc := &admissionv1.MutatingWebhookConfiguration{}
	if err := r.TargetClient.Get(ctx, types.NamespacedName{Name: req.Name}, mwc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get MutatingWebhookConfiguration %s: %w", req.Name, err)
	}

	// Re-check the label here as well — the cache predicate gates *events*,
	// but a delete-then-recreate without labels could still leak through if
	// we relied on it alone.
	if mwc.Labels[r.WebhookLabelKey] != r.WebhookLabelValue {
		return ctrl.Result{}, nil
	}

	caChanged := false
	rewroteURL := ""
	for j := range mwc.Webhooks {
		cc := &mwc.Webhooks[j].ClientConfig
		if !bytes.Equal(cc.CABundle, caCert) {
			caChanged = true
		}
		cc.CABundle = caCert
		if r.ExternalHost != "" && cc.Service != nil {
			url := webhookhost.BuildExternalURL(r.ExternalHost, cc.Service.Path)
			cc.URL = &url
			cc.Service = nil
			rewroteURL = url
		}
	}
	if !caChanged && rewroteURL == "" {
		// Already converged — likely a Watch event from our own previous
		// stamp. Skip the Update to avoid bouncing resourceVersion.
		return ctrl.Result{}, nil
	}

	if err := r.TargetClient.Update(ctx, mwc); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update MutatingWebhookConfiguration %s: %w", mwc.Name, err)
	}

	if rewroteURL != "" {
		logger.Info("Stamped MutatingWebhookConfiguration", "webhooks", len(mwc.Webhooks), "rewroteURL", rewroteURL, "caBundleChanged", caChanged)
	} else {
		logger.Info("Stamped MutatingWebhookConfiguration", "webhooks", len(mwc.Webhooks), "caBundleChanged", caChanged)
	}
	return ctrl.Result{}, nil
}

// reconcileVWC mirrors reconcileMWC for ValidatingWebhookConfiguration.
func (r *TargetWebhookReconciler) reconcileVWC(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", "ValidatingWebhookConfiguration", "name", req.Name)

	caCert, err := r.loadCABundle(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	vwc := &admissionv1.ValidatingWebhookConfiguration{}
	if err := r.TargetClient.Get(ctx, types.NamespacedName{Name: req.Name}, vwc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get ValidatingWebhookConfiguration %s: %w", req.Name, err)
	}

	if vwc.Labels[r.WebhookLabelKey] != r.WebhookLabelValue {
		return ctrl.Result{}, nil
	}

	caChanged := false
	rewroteURL := ""
	for j := range vwc.Webhooks {
		cc := &vwc.Webhooks[j].ClientConfig
		if !bytes.Equal(cc.CABundle, caCert) {
			caChanged = true
		}
		cc.CABundle = caCert
		if r.ExternalHost != "" && cc.Service != nil {
			url := webhookhost.BuildExternalURL(r.ExternalHost, cc.Service.Path)
			cc.URL = &url
			cc.Service = nil
			rewroteURL = url
		}
	}
	if !caChanged && rewroteURL == "" {
		// Already converged — likely a Watch event from our own previous
		// stamp. Skip the Update to avoid bouncing resourceVersion.
		return ctrl.Result{}, nil
	}

	if err := r.TargetClient.Update(ctx, vwc); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
	}

	if rewroteURL != "" {
		logger.Info("Stamped ValidatingWebhookConfiguration", "webhooks", len(vwc.Webhooks), "rewroteURL", rewroteURL, "caBundleChanged", caChanged)
	} else {
		logger.Info("Stamped ValidatingWebhookConfiguration", "webhooks", len(vwc.Webhooks), "caBundleChanged", caChanged)
	}
	return ctrl.Result{}, nil
}

// reconcileCRD stamps caBundle and (optionally) rewrites Service-form
// clientConfig on a single CustomResourceDefinition's conversion webhook in
// the target cluster. Skips CRDs without a Webhook conversion strategy.
//
// Unlike reconcileMWC/reconcileVWC, this path frequently fires when nothing
// has changed: the admission webhook stamps CRDs synchronously on
// CREATE/UPDATE, so a follow-up Watch event for our own write would be a
// no-op. The body short-circuits in that case to avoid a needless Update
// (and the resulting resourceVersion bump that would re-trigger us).
func (r *TargetWebhookReconciler) reconcileCRD(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", "CustomResourceDefinition", "name", req.Name)

	caCert, err := r.loadCABundle(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.TargetClient.Get(ctx, types.NamespacedName{Name: req.Name}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get CustomResourceDefinition %s: %w", req.Name, err)
	}

	if crd.Labels[r.WebhookLabelKey] != r.WebhookLabelValue {
		return ctrl.Result{}, nil
	}

	conv := crd.Spec.Conversion
	if conv == nil || conv.Strategy != apiextensionsv1.WebhookConverter || conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		logger.V(1).Info("CRD has no Webhook conversion strategy, skipping")
		return ctrl.Result{}, nil
	}

	cc := conv.Webhook.ClientConfig
	caChanged := !bytes.Equal(cc.CABundle, caCert)
	rewroteURL := ""
	if r.ExternalHost != "" && cc.Service != nil {
		url := webhookhost.BuildExternalURL(r.ExternalHost, cc.Service.Path)
		cc.URL = &url
		cc.Service = nil
		rewroteURL = url
	}
	if !caChanged && rewroteURL == "" {
		// Already converged — likely a Watch event from our own admission
		// webhook stamp. Skip the Update to avoid bouncing resourceVersion.
		return ctrl.Result{}, nil
	}
	cc.CABundle = caCert

	if err := r.TargetClient.Update(ctx, crd); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update CustomResourceDefinition %s: %w", crd.Name, err)
	}

	if rewroteURL != "" {
		logger.Info("Stamped CustomResourceDefinition conversion webhook", "rewroteURL", rewroteURL, "caBundleChanged", caChanged)
	} else {
		logger.Info("Stamped CustomResourceDefinition conversion webhook", "caBundleChanged", caChanged)
	}
	return ctrl.Result{}, nil
}

// loadCABundle reads tls.crt from the hosting-cluster certificate Secret. An
// empty or missing tls.crt is returned as an error so the controller retries
// with backoff — the WebhookReconciler is on a different reconcile loop and
// will populate the Secret shortly after startup.
func (r *TargetWebhookReconciler) loadCABundle(ctx context.Context) ([]byte, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: r.CertSecretName, Namespace: r.Namespace}
	if err := r.HostingClient.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("failed to get cert Secret %s: %w", key, err)
	}
	caCert := secret.Data[tlsCrtKey]
	if len(caCert) == 0 {
		return nil, errors.New("cert Secret has empty tls.crt; retrying")
	}
	return caCert, nil
}
