// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

// Package admission implements a mutating admission handler that runs in
// patch mode alongside the controller's list-and-patch reconcile loop. For
// every CREATE/UPDATE on a labeled MutatingWebhookConfiguration,
// ValidatingWebhookConfiguration, or CustomResourceDefinition, the handler
// stamps clientConfig.CABundle with the current self-signed certificate and
// (when ExternalHost is set) rewrites Service-form clientConfig to URL-form
// pointing at https://<ExternalHost><Service.Path>.
//
// The handler is registered at three paths (/mutate-mwc, /mutate-vwc,
// /mutate-crd) so the cluster's MWC for our own webhook can declare distinct
// rules per kind, but a single Mutator instance backs all three.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/webhookhost"
)

// CABundleSource returns the current PEM-encoded CA certificate to inject.
// It is read on every admission request so cert rotations are picked up
// without restarting the webhook server. Returning an empty slice causes the
// handler to allow the object through unchanged, since stamping an empty
// caBundle would weaken existing entries.
type CABundleSource func(ctx context.Context) ([]byte, error)

// Admission Kind values used to dispatch req.Kind.Kind. Exported so tests
// (and the in-process integration test) can reference them by name instead
// of repeating string literals.
const (
	KindMutatingWebhookConfiguration   = "MutatingWebhookConfiguration"
	KindValidatingWebhookConfiguration = "ValidatingWebhookConfiguration"
	KindCustomResourceDefinition       = "CustomResourceDefinition"
)

// Mutator is the controller-runtime admission handler.
type Mutator struct {
	// Decoder decodes req.Object into typed Go objects. Construct via
	// admission.NewDecoder with a scheme that has admissionregistration/v1
	// and apiextensions/v1 registered.
	Decoder admission.Decoder

	// ExternalHost, when non-empty, causes Service-form clientConfig entries
	// to be rewritten to URL-form. Matches the controller's --external-host
	// flag semantics.
	ExternalHost string

	// CABundle returns the current PEM cert. Required.
	CABundle CABundleSource

	// SelfMWCName is the name of the MutatingWebhookConfiguration the
	// controller bootstraps to register this handler. Requests targeting an
	// MWC with this name are short-circuited to admission.Allowed so we
	// cannot mutate ourselves even if some external actor manages to make
	// our MWC match its own objectSelector. The bootstrap MWC is also
	// stamped with empty Labels for the same reason; this is a second line
	// of defense. Empty disables the guard (e.g., in unit tests).
	SelfMWCName string
}

// Handle implements admission.Handler.
func (m *Mutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues(
		"kind", req.Kind.Kind,
		"name", req.Name,
		"operation", req.Operation,
	)

	caBundle, err := m.CABundle(ctx)
	if err != nil {
		logger.Error(err, "failed to read CA bundle, allowing unchanged")
		return admission.Allowed("ca bundle unavailable, skipping mutation")
	}
	if len(caBundle) == 0 {
		logger.Info("CA bundle empty, allowing unchanged")
		return admission.Allowed("ca bundle empty, skipping mutation")
	}

	switch req.Kind.Kind {
	case KindMutatingWebhookConfiguration:
		if m.SelfMWCName != "" && req.Name == m.SelfMWCName {
			logger.Info("skipping self admission MWC by name")
			return admission.Allowed("self admission MWC, skipping")
		}
		return m.mutateMWC(ctx, req, caBundle)
	case KindValidatingWebhookConfiguration:
		return m.mutateVWC(ctx, req, caBundle)
	case KindCustomResourceDefinition:
		return m.mutateCRD(ctx, req, caBundle)
	default:
		return admission.Allowed(fmt.Sprintf("unhandled kind %q", req.Kind.Kind))
	}
}

func (m *Mutator) mutateMWC(ctx context.Context, req admission.Request, caBundle []byte) admission.Response {
	logger := log.FromContext(ctx)
	obj := &admissionv1.MutatingWebhookConfiguration{}
	if err := m.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	rewrites := 0
	for i := range obj.Webhooks {
		cc := &obj.Webhooks[i].ClientConfig
		cc.CABundle = caBundle
		if m.ExternalHost != "" && cc.Service != nil {
			url := webhookhost.BuildExternalURL(m.ExternalHost, cc.Service.Path)
			cc.URL = &url
			cc.Service = nil
			rewrites++
		}
	}
	logger.Info("mutated MutatingWebhookConfiguration",
		"name", req.Name, "webhooks", len(obj.Webhooks),
		"serviceToURLRewrites", rewrites, "externalHost", m.ExternalHost,
	)
	return marshalPatch(req, obj)
}

func (m *Mutator) mutateVWC(ctx context.Context, req admission.Request, caBundle []byte) admission.Response {
	logger := log.FromContext(ctx)
	obj := &admissionv1.ValidatingWebhookConfiguration{}
	if err := m.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	rewrites := 0
	for i := range obj.Webhooks {
		cc := &obj.Webhooks[i].ClientConfig
		cc.CABundle = caBundle
		if m.ExternalHost != "" && cc.Service != nil {
			url := webhookhost.BuildExternalURL(m.ExternalHost, cc.Service.Path)
			cc.URL = &url
			cc.Service = nil
			rewrites++
		}
	}
	logger.Info("mutated ValidatingWebhookConfiguration",
		"name", req.Name, "webhooks", len(obj.Webhooks),
		"serviceToURLRewrites", rewrites, "externalHost", m.ExternalHost,
	)
	return marshalPatch(req, obj)
}

func (m *Mutator) mutateCRD(ctx context.Context, req admission.Request, caBundle []byte) admission.Response {
	logger := log.FromContext(ctx)
	obj := &apiextensionsv1.CustomResourceDefinition{}
	if err := m.Decoder.Decode(req, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	conv := obj.Spec.Conversion
	if conv == nil || conv.Strategy != apiextensionsv1.WebhookConverter || conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		logger.Info("CRD has no Webhook conversion strategy, allowing unchanged", "name", req.Name)
		return admission.Allowed("CRD has no Webhook conversion strategy")
	}
	cc := conv.Webhook.ClientConfig
	cc.CABundle = caBundle
	rewritten := false
	if m.ExternalHost != "" && cc.Service != nil {
		url := webhookhost.BuildExternalURL(m.ExternalHost, cc.Service.Path)
		cc.URL = &url
		cc.Service = nil
		rewritten = true
	}
	logger.Info("mutated CustomResourceDefinition conversion webhook",
		"name", req.Name, "serviceToURLRewritten", rewritten, "externalHost", m.ExternalHost,
	)
	return marshalPatch(req, obj)
}

func marshalPatch(req admission.Request, mutated any) admission.Response {
	raw, err := json.Marshal(mutated)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}
