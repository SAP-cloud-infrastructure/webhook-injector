// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
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

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/metrics"
)

// TargetWebhookReconciler keeps caBundle in sync on CRD conversion webhooks
// and Mutating/ValidatingWebhookConfigurations applied DIRECTLY to the target
// cluster (GRM-free / direct-apply topology). Opt-in via --target-label.
// Leader-only, event-driven via a label-scoped target-cluster cache.
//
// Write surface is minimal: patches ONLY caBundle (MergeFrom for the CRD's
// nested clientConfig; StrategicMergeFrom for MWC/VWC so per-webhook entries
// merge by name instead of replacing the whole list), never creates/deletes,
// never rewrites clientConfig Service/URL, and only touches labeled objects —
// so it never fights GRM (GRM-owned objects lack the label). Stamped CRD names
// are published on the shared rotation Gate so WebhookReconciler's staged-phase
// promotion waits for target propagation; MWC/VWC are not gated (they are not
// part of conversion-caBundle gating).
type TargetWebhookReconciler struct {
	TargetClient     client.Client
	CertSecretReader client.Reader
	Namespace        string
	CertSecretName   string
	LabelKey         string
	LabelValue       string
	Gate             *rotationgate.Gate
}

func (r *TargetWebhookReconciler) certReader() client.Reader {
	if r.CertSecretReader != nil {
		return r.CertSecretReader
	}
	return r.TargetClient
}

func (r *TargetWebhookReconciler) hasLabel(obj client.Object) bool {
	return obj.GetLabels()[r.LabelKey] == r.LabelValue
}

// targetKindMWC / targetKindVWC / targetKindCRD are the metrics-label kind
// values for patch-mode admission-webhook stamps (TargetStampsTotal reuses the
// first two, matching the ConfigMap path's kinds) and for the rotation
// fan-out (RotationFanOutEventsTotal, all three).
const (
	targetKindCRD = "TargetCRD"
	targetKindMWC = "MutatingWebhookConfiguration"
	targetKindVWC = "ValidatingWebhookConfiguration"

	// fanOutKindMWC / fanOutKindVWC are the RotationFanOutEventsTotal kind
	// values for the target-cluster MWC/VWC resyncs. They differ from the
	// TargetStampsTotal kind values (which reuse the API kind name) so the
	// fan-out kind enum is uniformly TargetXxx across all three kinds and
	// cannot collide with the ManagedResource fan-out label.
	fanOutKindMWC = "TargetMWC"
	fanOutKindVWC = "TargetVWC"
)

// ReconcileCRD stamps caBundle on one labeled target CRD's conversion webhook.
func (r *TargetWebhookReconciler) ReconcileCRD(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", crdKind, "name", req.Name)
	gateKey := rotationgate.Key(req.Name)

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.TargetClient.Get(ctx, req.NamespacedName, crd); err != nil {
		if apierrors.IsNotFound(err) {
			if r.Gate != nil {
				r.Gate.Forget(gateKey)
			}
			return ctrl.Result{}, nil
		}
		metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("get target CRD %s: %w", req.Name, err)
	}
	if !r.hasLabel(crd) {
		if r.Gate != nil {
			r.Gate.Forget(gateKey)
		}
		return ctrl.Result{}, nil
	}
	conv := crd.Spec.Conversion
	if conv == nil || conv.Strategy != apiextensionsv1.WebhookConverter ||
		conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		// No longer a Webhook-conversion CRD we control — drop any gate
		// entry a prior pass published so a future rotation doesn't wait
		// on it until --rotation-gate-timeout.
		if r.Gate != nil {
			r.Gate.Forget(gateKey)
		}
		return ctrl.Result{}, nil
	}
	caCert, err := loadCABundle(ctx, r.certReader(), r.Namespace, r.CertSecretName)
	if err != nil {
		metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultError).Inc()
		return ctrl.Result{}, err
	}
	if r.Gate != nil {
		r.Gate.Publish(gateKey, []string{crd.Name})
	}
	base := crd.DeepCopy()
	if !stampCRDConversionCABundle(crd, caCert) {
		metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultConverged).Inc()
		return ctrl.Result{}, nil
	}
	if err := r.TargetClient.Patch(ctx, crd, client.MergeFrom(base)); err != nil {
		metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("patch target CRD %s caBundle: %w", crd.Name, err)
	}
	metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultStamped).Inc()
	logger.Info("Stamped target CRD conversion caBundle")
	return ctrl.Result{}, nil
}

// ReconcileMWC stamps caBundle on one labeled target MutatingWebhookConfiguration.
func (r *TargetWebhookReconciler) ReconcileMWC(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", targetKindMWC, "name", req.Name)
	mwc := &admissionv1.MutatingWebhookConfiguration{}
	if err := r.TargetClient.Get(ctx, req.NamespacedName, mwc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("get target MWC %s: %w", req.Name, err)
	}
	if !r.hasLabel(mwc) {
		return ctrl.Result{}, nil
	}
	caCert, err := loadCABundle(ctx, r.certReader(), r.Namespace, r.CertSecretName)
	if err != nil {
		metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultError).Inc()
		return ctrl.Result{}, err
	}
	base := mwc.DeepCopy()
	if !stampMutatingCABundle(mwc, caCert) {
		metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultConverged).Inc()
		return ctrl.Result{}, nil
	}
	if err := r.TargetClient.Patch(ctx, mwc, client.StrategicMergeFrom(base)); err != nil {
		metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("patch target MWC %s caBundle: %w", req.Name, err)
	}
	metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultStamped).Inc()
	logger.Info("Stamped target MutatingWebhookConfiguration caBundle")
	return ctrl.Result{}, nil
}

// ReconcileVWC stamps caBundle on one labeled target ValidatingWebhookConfiguration.
func (r *TargetWebhookReconciler) ReconcileVWC(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", targetKindVWC, "name", req.Name)
	vwc := &admissionv1.ValidatingWebhookConfiguration{}
	if err := r.TargetClient.Get(ctx, req.NamespacedName, vwc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("get target VWC %s: %w", req.Name, err)
	}
	if !r.hasLabel(vwc) {
		return ctrl.Result{}, nil
	}
	caCert, err := loadCABundle(ctx, r.certReader(), r.Namespace, r.CertSecretName)
	if err != nil {
		metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultError).Inc()
		return ctrl.Result{}, err
	}
	base := vwc.DeepCopy()
	if !stampValidatingCABundle(vwc, caCert) {
		metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultConverged).Inc()
		return ctrl.Result{}, nil
	}
	if err := r.TargetClient.Patch(ctx, vwc, client.StrategicMergeFrom(base)); err != nil {
		metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("patch target VWC %s caBundle: %w", req.Name, err)
	}
	metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultStamped).Inc()
	logger.Info("Stamped target ValidatingWebhookConfiguration caBundle")
	return ctrl.Result{}, nil
}

// SetupWithManager registers three leader-only controllers (target-crd,
// target-mwc, target-vwc) that watch labeled objects on the TARGET cluster's
// cache. Because those objects live on a different cluster than the manager,
// each watch uses source.Kind against targetCluster.GetCache(). A resync
// trigger (fed by rotationTrigger and firing once at startup) re-lists all
// labeled objects so a fresh caBundle propagates immediately and the initial
// state is reconciled even with an empty event backlog.
func (r *TargetWebhookReconciler) SetupWithManager(mgr ctrl.Manager, targetCluster cluster.Cluster, rotationTrigger <-chan struct{}) error {
	crdCh := make(chan event.GenericEvent, 64)
	mwcCh := make(chan event.GenericEvent, 64)
	vwcCh := make(chan event.GenericEvent, 64)

	trigger := &targetResyncTrigger{
		targetClient:    r.TargetClient,
		labelKey:        r.LabelKey,
		labelValue:      r.LabelValue,
		rotationTrigger: rotationTrigger,
		crdCh:           crdCh,
		mwcCh:           mwcCh,
		vwcCh:           vwcCh,
	}
	if err := mgr.Add(trigger); err != nil {
		return fmt.Errorf("register target resync trigger: %w", err)
	}

	labelPred := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj != nil && obj.GetLabels()[r.LabelKey] == r.LabelValue
	})
	nameMapper := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: obj.GetName()}}}
	})
	tc := targetCluster.GetCache()

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("target-crd").
		WatchesRawSource(source.Kind(tc, client.Object(&apiextensionsv1.CustomResourceDefinition{}), &handler.EnqueueRequestForObject{}, labelPred)).
		WatchesRawSource(source.Channel(crdCh, nameMapper)).
		Complete(reconcile.Func(r.ReconcileCRD)); err != nil {
		return fmt.Errorf("build target-crd controller: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("target-mwc").
		WatchesRawSource(source.Kind(tc, client.Object(&admissionv1.MutatingWebhookConfiguration{}), &handler.EnqueueRequestForObject{}, labelPred)).
		WatchesRawSource(source.Channel(mwcCh, nameMapper)).
		Complete(reconcile.Func(r.ReconcileMWC)); err != nil {
		return fmt.Errorf("build target-mwc controller: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("target-vwc").
		WatchesRawSource(source.Kind(tc, client.Object(&admissionv1.ValidatingWebhookConfiguration{}), &handler.EnqueueRequestForObject{}, labelPred)).
		WatchesRawSource(source.Channel(vwcCh, nameMapper)).
		Complete(reconcile.Func(r.ReconcileVWC)); err != nil {
		return fmt.Errorf("build target-vwc controller: %w", err)
	}
	return nil
}

// targetResyncTrigger lists labeled CRD/MWC/VWC on the target cluster and
// emits one GenericEvent per object on the matching channel — once at startup
// and on each rotation signal. Leader-only.
type targetResyncTrigger struct {
	targetClient    client.Client
	labelKey        string
	labelValue      string
	rotationTrigger <-chan struct{}
	crdCh           chan event.GenericEvent
	mwcCh           chan event.GenericEvent
	vwcCh           chan event.GenericEvent
}

func (t *targetResyncTrigger) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("target-resync-trigger")
	t.fanOut(ctx, logger) // initial pass so startup state is reconciled
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.rotationTrigger:
			t.fanOut(ctx, logger)
		}
	}
}

// fanOut lists every labeled CRD/MWC/VWC on the target cluster and emits one
// GenericEvent per object on the matching channel. Each kind runs in its own
// goroutine so a wedged or slow consumer for one kind cannot starve the
// others — the earlier sequential implementation head-of-line-blocked
// MWC/VWC resync whenever >64 labeled CRDs existed and the target-crd
// controller was momentarily slow. The three goroutines are independent
// and List failures are per-kind; a failure on one kind logs plus
// increments RotationFanOutEventsTotal{outcome="error"} but never
// prevents the other two kinds from resyncing.
func (t *targetResyncTrigger) fanOut(ctx context.Context, logger logr.Logger) {
	sel := labels.SelectorFromSet(labels.Set{t.labelKey: t.labelValue})
	opt := client.MatchingLabelsSelector{Selector: sel}

	var wg sync.WaitGroup
	wg.Add(3)
	go t.fanOutCRDs(ctx, logger, opt, &wg)
	go t.fanOutMWCs(ctx, logger, opt, &wg)
	go t.fanOutVWCs(ctx, logger, opt, &wg)
	wg.Wait()
}

func (t *targetResyncTrigger) fanOutCRDs(ctx context.Context, logger logr.Logger, opt client.MatchingLabelsSelector, wg *sync.WaitGroup) {
	defer wg.Done()
	var crds apiextensionsv1.CustomResourceDefinitionList
	if err := t.targetClient.List(ctx, &crds, opt); err != nil {
		logger.Error(err, "target resync: list CRDs")
		metrics.RotationFanOutEventsTotal.WithLabelValues(targetKindCRD, metrics.FanOutError).Inc()
		return
	}
	for i := range crds.Items {
		if !t.send(ctx, t.crdCh, &crds.Items[i]) {
			return
		}
		metrics.RotationFanOutEventsTotal.WithLabelValues(targetKindCRD, metrics.FanOutEmitted).Inc()
	}
	logger.V(1).Info("target resync: CRDs complete", "count", len(crds.Items))
}

func (t *targetResyncTrigger) fanOutMWCs(ctx context.Context, logger logr.Logger, opt client.MatchingLabelsSelector, wg *sync.WaitGroup) {
	defer wg.Done()
	var mwcs admissionv1.MutatingWebhookConfigurationList
	if err := t.targetClient.List(ctx, &mwcs, opt); err != nil {
		logger.Error(err, "target resync: list MWCs")
		metrics.RotationFanOutEventsTotal.WithLabelValues(fanOutKindMWC, metrics.FanOutError).Inc()
		return
	}
	for i := range mwcs.Items {
		if !t.send(ctx, t.mwcCh, &mwcs.Items[i]) {
			return
		}
		metrics.RotationFanOutEventsTotal.WithLabelValues(fanOutKindMWC, metrics.FanOutEmitted).Inc()
	}
	logger.V(1).Info("target resync: MWCs complete", "count", len(mwcs.Items))
}

func (t *targetResyncTrigger) fanOutVWCs(ctx context.Context, logger logr.Logger, opt client.MatchingLabelsSelector, wg *sync.WaitGroup) {
	defer wg.Done()
	var vwcs admissionv1.ValidatingWebhookConfigurationList
	if err := t.targetClient.List(ctx, &vwcs, opt); err != nil {
		logger.Error(err, "target resync: list VWCs")
		metrics.RotationFanOutEventsTotal.WithLabelValues(fanOutKindVWC, metrics.FanOutError).Inc()
		return
	}
	for i := range vwcs.Items {
		if !t.send(ctx, t.vwcCh, &vwcs.Items[i]) {
			return
		}
		metrics.RotationFanOutEventsTotal.WithLabelValues(fanOutKindVWC, metrics.FanOutEmitted).Inc()
	}
	logger.V(1).Info("target resync: VWCs complete", "count", len(vwcs.Items))
}

func (t *targetResyncTrigger) send(ctx context.Context, ch chan event.GenericEvent, obj client.Object) bool {
	select {
	case ch <- event.GenericEvent{Object: obj}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (t *targetResyncTrigger) NeedLeaderElection() bool { return true }

var (
	_ manager.Runnable               = (*targetResyncTrigger)(nil)
	_ manager.LeaderElectionRunnable = (*targetResyncTrigger)(nil)
)
