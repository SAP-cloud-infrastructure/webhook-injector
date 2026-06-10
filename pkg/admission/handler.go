// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

// Package admission implements a mutating admission handler for
// CustomResourceDefinition resources only. For every CREATE/UPDATE on a
// labeled CRD with a Webhook conversion strategy, the handler stamps
// spec.conversion.webhook.clientConfig.CABundle with the current self-signed
// certificate and (when ExternalHost is set) rewrites Service-form
// clientConfig to URL-form pointing at https://<ExternalHost><Service.Path>.
//
// MutatingWebhookConfiguration and ValidatingWebhookConfiguration resources
// are intentionally NOT handled here: kube-apiserver self-protects against
// admission webhooks on those kinds (see
// rules.IsExemptAdmissionConfigurationResource), so an admission webhook
// targeting them never fires in real clusters. MWC/VWC reconciliation is
// instead performed by a target-cluster Watch reconciler that lists and
// patches them directly.
package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

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
	KindCustomResourceDefinition = "CustomResourceDefinition"
)

// Mutator is the controller-runtime admission handler.
type Mutator struct {
	// Decoder decodes req.Object into typed Go objects. Construct via
	// admission.NewDecoder with a scheme that has apiextensions/v1
	// registered.
	Decoder admission.Decoder

	// ExternalHost, when non-empty, causes Service-form clientConfig entries
	// to be rewritten to URL-form. Matches the controller's --external-host
	// flag semantics.
	ExternalHost string

	// CABundle returns the current PEM cert. Required.
	CABundle CABundleSource
}

// Handle implements admission.Handler.
func (m *Mutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues(
		"kind", req.Kind.Kind,
		"namespace", req.Namespace,
		"name", req.Name,
		"operation", req.Operation,
		"uid", string(req.UID),
		"dryRun", ptrBool(req.DryRun),
		"userInfo", req.UserInfo.Username,
	)
	logger.Info("admission request received")

	caBundle, err := m.CABundle(ctx)
	if err != nil {
		logger.Error(err, "failed to read CA bundle, allowing unchanged")
		return admission.Allowed("ca bundle unavailable, skipping mutation")
	}
	if len(caBundle) == 0 {
		logger.Info("CA bundle empty, allowing unchanged")
		return admission.Allowed("ca bundle empty, skipping mutation")
	}
	logger.V(1).Info("CA bundle resolved", "caBundleBytes", len(caBundle))

	switch req.Kind.Kind {
	case KindCustomResourceDefinition:
		return m.mutateCRD(ctx, req, caBundle)
	default:
		logger.Info("unhandled kind, allowing unchanged")
		return admission.Allowed(fmt.Sprintf("unhandled kind %q", req.Kind.Kind))
	}
}

func (m *Mutator) mutateCRD(ctx context.Context, req admission.Request, caBundle []byte) admission.Response {
	logger := log.FromContext(ctx).WithValues("kind", KindCustomResourceDefinition, "name", req.Name)
	obj := &apiextensionsv1.CustomResourceDefinition{}
	if err := m.Decoder.Decode(req, obj); err != nil {
		logger.Error(err, "failed to decode CustomResourceDefinition")
		return admission.Errored(http.StatusBadRequest, err)
	}
	conv := obj.Spec.Conversion
	if conv == nil || conv.Strategy != apiextensionsv1.WebhookConverter || conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		logger.Info("CRD has no Webhook conversion strategy, allowing unchanged",
			"strategy", crdConversionStrategy(conv),
		)
		return admission.Allowed("CRD has no Webhook conversion strategy")
	}
	cc := conv.Webhook.ClientConfig
	caChanged := !bytes.Equal(cc.CABundle, caBundle)
	cc.CABundle = caBundle
	rewritten := false
	if m.ExternalHost != "" && cc.Service != nil {
		svc := cc.Service
		url := webhookhost.BuildExternalURL(m.ExternalHost, svc.Path)
		logger.Info("rewriting CRD conversion Service-form clientConfig to URL",
			"serviceNamespace", svc.Namespace, "serviceName", svc.Name,
			"servicePath", ptrString(svc.Path), "url", url,
			"caBundleChanged", caChanged,
		)
		cc.URL = &url
		cc.Service = nil
		rewritten = true
	} else {
		logger.V(1).Info("CRD conversion clientConfig left as-is",
			"hasURL", cc.URL != nil, "hasService", cc.Service != nil,
			"externalHostSet", m.ExternalHost != "", "caBundleChanged", caChanged,
		)
	}
	logger.Info("mutated CustomResourceDefinition conversion webhook",
		"serviceToURLRewritten", rewritten,
		"caBundleChanged", caChanged,
		"externalHost", m.ExternalHost,
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

func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func crdConversionStrategy(conv *apiextensionsv1.CustomResourceConversion) string {
	if conv == nil {
		return ""
	}
	return string(conv.Strategy)
}
