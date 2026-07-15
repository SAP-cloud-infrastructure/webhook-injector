// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/metrics"
)

// TargetWebhookReconciler keeps caBundle in sync on CRD conversion webhooks
// and Mutating/ValidatingWebhookConfigurations applied DIRECTLY to the target
// cluster (GRM-free / direct-apply topology). Opt-in via --target-crd-label.
// Leader-only, event-driven via a label-scoped target-cluster cache.
//
// Write surface is minimal: patches ONLY caBundle (MergeFrom), never
// creates/deletes, never rewrites clientConfig Service/URL, and only touches
// labeled objects — so it never fights GRM (GRM-owned objects lack the label).
// Stamped CRD names are published on the shared rotation Gate so
// WebhookReconciler's staged-phase promotion waits for target propagation;
// MWC/VWC are not gated (they are not part of conversion-caBundle gating).
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

// targetKindMWC / targetKindVWC are the TargetStampsTotal kind label values
// for patch-mode admission-webhook stamps (they reuse that counter, matching
// the ConfigMap path's kinds).
const (
	targetKindMWC = "MutatingWebhookConfiguration"
	targetKindVWC = "ValidatingWebhookConfiguration"
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
	if err := r.TargetClient.Patch(ctx, mwc, client.MergeFrom(base)); err != nil {
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
	if err := r.TargetClient.Patch(ctx, vwc, client.MergeFrom(base)); err != nil {
		metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("patch target VWC %s caBundle: %w", req.Name, err)
	}
	metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultStamped).Inc()
	logger.Info("Stamped target ValidatingWebhookConfiguration caBundle")
	return ctrl.Result{}, nil
}
