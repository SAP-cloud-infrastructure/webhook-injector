// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Result label values shared across multiple metrics. Centralised so the
// allowed cardinality is documented in one place — adding a new value
// implicitly extends the set of series Prometheus stores.
const (
	// ResultStamped is the "we wrote a change" outcome — the reconciler
	// actually mutated the target object.
	ResultStamped = "stamped"
	// ResultConverged is the "already in desired state" no-op outcome — the
	// reconciler observed the object but did not need to write. Used by the
	// idempotency-skip path in managed_resource_reconciler.
	ResultConverged = "converged"
	// ResultError is the "we tried and failed" outcome — used for both
	// transient errors (Get failed, Update failed) and decoding failures
	// inside an MR Secret. Lets a single panel show real failures without
	// inflating the converged-path counter.
	ResultError = "error"

	// FanOutEmitted — an event was successfully pushed onto the per-kind
	// channel feeding the controller. The current fanOut implementation
	// blocks until the consumer drains a slot rather than dropping, so
	// this is the only outcome value emitted in steady state.
	FanOutEmitted = "emitted"

	// ForceReconcilePatched — patched gardener.cloud/operation=reconcile
	// onto the MR after a successful Secret stamp.
	ForceReconcilePatched = "patched"
	// ForceReconcileSkipped — patch skipped because the annotation was
	// already present (a previous reconcile set it, GRM has not yet
	// consumed it). Avoids no-op MR writes.
	ForceReconcileSkipped = "skipped"
	// ForceReconcileError — patch failed. The reconcile itself does not
	// fail when this happens (we logged and moved on), so this counter is
	// the only signal that GRM may be running on its own Secret-watch
	// cadence instead of the kicked one.
	ForceReconcileError = "error"

	// EncodingPlain — uncompressed YAML key inside an MR Secret.
	EncodingPlain = "plain"
	// EncodingBrotli — `.br`-suffixed key handled via the brotli codec.
	EncodingBrotli = "brotli"
)

var (
	// CertificateRotationsTotal counts certificate rotations by reason.
	// reason label values: "initial" (first generation), "expired" (past threshold),
	// "san_mismatch" (service references changed), "force_rotation" (annotation trigger).
	CertificateRotationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_injector_certificate_rotations_total",
			Help: "Total number of certificate rotations, labeled by reason.",
		},
		[]string{"reason"},
	)

	// CertificateExpiryTime reports the Unix timestamp at which the current certificate expires.
	CertificateExpiryTime = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "webhook_injector_certificate_expiry_time_seconds",
			Help: "Unix timestamp when the current TLS certificate expires.",
		},
	)

	// CertFilesWrittenTotal counts how many times certificate files were written to disk
	// by the CertificateReloader. A spike here indicates active cert propagation.
	CertFilesWrittenTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "webhook_injector_cert_files_written_total",
			Help: "Total number of times certificate files were written to disk by the reloader.",
		},
	)

	// TargetStampsTotal counts reconcile outcomes on target-cluster
	// MutatingWebhookConfiguration / ValidatingWebhookConfiguration objects
	// authored by ConfigMap mode, partitioned by kind and outcome.
	// "stamped" = we wrote caBundle/URL or otherwise reapplied the doc;
	// "converged" = no-op (already in desired state); "error" = Get/Update
	// failure.
	TargetStampsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_injector_target_stamps_total",
			Help: "Total reconcile outcomes for target-cluster MWC/VWC stamping, labeled by kind and result (stamped|converged|error).",
		},
		[]string{"kind", "result"},
	)

	// RotationFanOutEventsTotal counts per-kind events emitted from the
	// rotation fan-out goroutines on every cert rotation. The fanOut
	// producer blocks until the consumer drains, so the only outcome
	// value emitted in steady state is "emitted"; the "outcome" label
	// is retained for forward compatibility if a non-blocking variant
	// is introduced again.
	RotationFanOutEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_injector_rotation_fanout_events_total",
			Help: "Total events fanned out to per-kind reconcile channels on certificate rotation, labeled by kind and outcome (emitted).",
		},
		[]string{"kind", "outcome"},
	)

	// ManagedResourceStampsTotal counts reconcile outcomes on the
	// hosting-cluster ManagedResource controller, partitioned by result.
	// "stamped" = at least one referenced Secret was actually mutated;
	// "converged" = all referenced Secrets were already in desired state;
	// "error" = Get/decode/Update failure.
	ManagedResourceStampsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_injector_managed_resource_stamps_total",
			Help: "Total reconcile outcomes for ManagedResource embedded-CRD stamping, labeled by result (stamped|converged|error).",
		},
		[]string{"result"},
	)

	// ManagedResourceCRDsStampedTotal counts individual CRD documents
	// rewritten inside MR Secrets. Distinct from ManagedResourceStampsTotal
	// because a single MR reconcile may stamp multiple CRDs in multiple
	// Secrets, and the per-CRD signal is what you want for a "how much
	// work did the rotation actually do" panel.
	ManagedResourceCRDsStampedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "webhook_injector_managed_resource_crds_stamped_total",
			Help: "Total CRD documents inside Gardener ManagedResource Secrets that were rewritten with caBundle or URL.",
		},
	)

	// ManagedResourceSecretKeysTotal counts every Secret data key the MR
	// reconciler processed, partitioned by encoding. Lets dashboards
	// confirm the brotli codec path is exercised against the production
	// fleet — a seed running only plain-YAML MRs would show
	// encoding="brotli" at zero.
	ManagedResourceSecretKeysTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_injector_managed_resource_secret_keys_total",
			Help: "Total Secret data keys processed by the ManagedResource reconciler, labeled by encoding (plain|brotli).",
		},
		[]string{"encoding"},
	)

	// ManagedResourceForceReconcileTotal counts patches of the
	// gardener.cloud/operation=reconcile annotation onto an MR, partitioned
	// by outcome. "patched" = annotation actually written; "skipped" =
	// already present (GRM hasn't consumed it yet); "error" = patch
	// failed (reconcile itself does not fail in that case — this counter
	// is the sole signal of degraded GRM-kick).
	ManagedResourceForceReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_injector_managed_resource_force_reconcile_total",
			Help: "Total gardener.cloud/operation=reconcile annotation patches on ManagedResources, labeled by result (patched|skipped|error).",
		},
		[]string{"result"},
	)

	// ManagedResourceKeepObjectsViolationsTotal counts reconciles that
	// observed at least one CRD inside the MR's referenced Secrets while
	// the MR's spec.keepObjects was not explicitly true. Without that
	// flag, deleting the ManagedResource cascade-deletes the shoot's CRDs
	// and every custom resource of those kinds — a destructive
	// misconfiguration the controller surfaces but does not block (we
	// still stamp the CRD so the webhook keeps working). One increment
	// per offending reconcile, not per CRD doc, so the metric measures
	// "how many MRs are misconfigured" rather than "how many CRDs are
	// at risk".
	ManagedResourceKeepObjectsViolationsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "webhook_injector_managed_resource_keep_objects_violations_total",
			Help: "Total reconciles of a labeled ManagedResource that ships at least one CRD but does not set spec.keepObjects=true.",
		},
	)

	// ManagedResourceUnhealthyTotal counts reconciles that observed a
	// ManagedResource whose ResourcesApplied or ResourcesHealthy condition
	// was present but not Status=True. Labels:
	//   condition ∈ {ResourcesApplied, ResourcesHealthy}
	//   status    ∈ {False, Progressing, Unknown}
	// Missing conditions (e.g. on a freshly created MR before GRM evaluates)
	// are intentionally NOT counted — the metric measures MRs that GRM has
	// evaluated and found non-True, not MRs GRM has been silent about.
	// One increment per condition per reconcile, so the rate is bounded by
	// the reconcile frequency.
	ManagedResourceUnhealthyTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_injector_managed_resource_unhealthy_total",
			Help: "Total reconciles that observed a labeled ManagedResource with ResourcesApplied or ResourcesHealthy not in Status=True, labeled by condition and observed status.",
		},
		[]string{"condition", "status"},
	)

	// ForceRotationDeferredTotal counts reconcile passes in which a
	// force-rotation annotation was observed but deferred because a
	// previous rotation cycle was still in flight (phase=staged, or
	// phase=active with the rotation-promoted-at marker present — i.e.
	// the overlap window has not yet elapsed). The annotation stays on
	// the Secret across these reconciles; once the prior rotation reaches
	// steady-state active the gate lifts and the force takes effect on
	// the next pass. A sustained non-zero rate means an operator is
	// retriggering rotations faster than they can complete; a single
	// brief spike is the normal "user forced during overlap window"
	// shape. Counter, not gauge — the rate is the signal.
	ForceRotationDeferredTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "webhook_injector_certificate_force_rotation_deferred_total",
			Help: "Total reconcile passes in which a force-rotation annotation was held back because a prior rotation cycle was still in flight.",
		},
	)

	// PhaseTransitionsTotal counts state-machine transitions of the
	// certificate-rotation phase on the cert Secret. Labels follow the
	// closed enum {absent, staged, active, pruned}. The pair (from, to)
	// makes the legal transitions in normal operation explicit:
	//   absent  → active  (initial bootstrap)
	//   active  → staged  (rotation begins: append new cert to bundle)
	//   staged  → active  (promote new key once shoot has both certs)
	//   active  → pruned  (overlap window elapsed; drop old cert)
	// Force-rotation observed during an in-flight cycle is deferred (see
	// ForceRotationDeferredTotal), so the previously-legal staged→staged
	// transition does not occur. Any other pair indicates a bug. Counter,
	// not gauge — phases are transient and rare; the rate over time is
	// the meaningful signal.
	PhaseTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_injector_certificate_rotation_phase_transitions_total",
			Help: "Total certificate-rotation phase transitions, labeled by from→to (enum: absent|active|staged|pruned).",
		},
		[]string{"from", "to"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		CertificateRotationsTotal,
		CertificateExpiryTime,
		CertFilesWrittenTotal,
		TargetStampsTotal,
		RotationFanOutEventsTotal,
		ManagedResourceStampsTotal,
		ManagedResourceCRDsStampedTotal,
		ManagedResourceSecretKeysTotal,
		ManagedResourceForceReconcileTotal,
		ManagedResourceKeepObjectsViolationsTotal,
		ManagedResourceUnhealthyTotal,
		ForceRotationDeferredTotal,
		PhaseTransitionsTotal,
	)
}
