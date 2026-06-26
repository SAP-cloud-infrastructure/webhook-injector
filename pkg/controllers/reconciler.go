// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/go-logr/logr"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/certificates"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/metrics"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/webhooks"
)

// rotationReason identifies why a certificate was rotated.
type rotationReason string

// RotationPhase tracks the state of an in-progress certificate rotation
// on the hosting-cluster cert Secret. It is persisted as an annotation
// (`webhook-injector.sap.com/rotation-phase`) so the state machine
// survives controller restarts and leader failovers.
//
// State transitions:
//
//	[absent]              ── initial reconcile ──►  active   (cert=A, key=A)
//	active   + rotate ──►  staged  (cert=A||B, key=A)         ← apiserver trusts both
//	staged   + propagated ──► active (cert=A||B, key=B)       ← serve new cert
//	active   + window elapsed ──► active (cert=B, key=B)      ← prune old cert
//
// `active` is the steady state when there is exactly one CERTIFICATE
// block in `tls.crt`. The same `active` label is used for "post-promote,
// pre-prune" (two blocks) — they differ in the number of CERTIFICATE
// blocks in `tls.crt` and the presence of the rotation-promoted-at
// marker, not in operational meaning. The phase is the resumable bit
// of state.
//
// Force-rotation interaction: a force-rotation annotation observed while
// the cycle is not yet at steady-state active (either phase=staged, or
// phase=active with rotation-promoted-at still set) is DEFERRED, not
// applied. The annotation stays on the Secret and takes effect on the
// first reconcile after the cycle settles. This guarantees a forced
// rotation never drops a cert before its overlap window expires. See
// isRotationInFlight and the gate in Reconcile.
type RotationPhase string

const (
	// ForceRotationAnnotation is the annotation key used to trigger certificate rotation.
	// When set to "true" on the certificate Secret, it forces immediate rotation.
	ForceRotationAnnotation = "webhook-injector.sap.com/force-rotation"

	// annotationTrue is the sentinel value that turns on a boolean
	// annotation in this package. Kept as a named constant rather than
	// repeating "true" so callers (production code and tests) speak the
	// same vocabulary, and so future renames are mechanical.
	annotationTrue = "true"

	// RotationPhaseAnnotation records the current phase of an in-progress
	// rotation on the cert Secret. Absent = steady state (equivalent to
	// PhaseActive). Values: "staged", "active".
	RotationPhaseAnnotation = "webhook-injector.sap.com/rotation-phase"

	// RotationStartedAtAnnotation is the RFC3339 timestamp at which the
	// current rotation began. Used to compute the prune deadline (active
	// phase + overlap window).
	RotationStartedAtAnnotation = "webhook-injector.sap.com/rotation-started-at"

	PhaseStaged RotationPhase = "staged"
	PhaseActive RotationPhase = "active"

	rotationReasonInitial     rotationReason = "initial"
	rotationReasonForced      rotationReason = "force_rotation"
	rotationReasonExpired     rotationReason = "expired"
	rotationReasonSANMismatch rotationReason = "san_mismatch"

	tlsCrtKey        = "tls.crt"
	tlsKeyKey        = "tls.key"
	tlsStagedKeyKey  = "tls.staged.key" // private key for the not-yet-served leaf during staged phase
	defaultNamespace = "default"

	defaultRotationOverlapWindow = 5 * time.Minute
	defaultRotationGateTimeout   = 2 * time.Minute
	stagedRequeueInterval        = 5 * time.Second // how often to poll target CRDs during staged phase
)

// WebhookReconciler reconciles a user-authored ConfigMap that carries
// MutatingWebhookConfiguration / ValidatingWebhookConfiguration YAML and
// manages the hosting-cluster certificate Secret. Apply-on-create, update
// on every reconcile pass, with self-signed certs generated and rotated
// here. CRDs are never authored by this controller — they flow through
// ManagedResourceReconciler instead.
type WebhookReconciler struct {
	Client       client.Client // Hosting cluster client
	TargetClient client.Client // Target cluster client

	// WebhookConfigName is the ConfigMap (in Namespace) that carries the user's
	// MWC/VWC YAML. Required — a startup-time validation error if unset.
	WebhookConfigName string

	// CertSANs is a list of DNS names and/or IP addresses to include as
	// additional certificate SANs. Each entry is auto-classified: parses
	// as net.ParseIP → IP SAN, otherwise DNS SAN. Optional; merged with
	// SANs derived from the ConfigMap's webhook services/URLs.
	CertSANs []string

	Namespace             string
	CertSecretName        string
	CertValidity          time.Duration
	CertRotationThreshold float64
	Scheme                *runtime.Scheme

	// RotationTrigger is signaled (non-blocking send) on every detected
	// certificate rotation. The sole downstream consumer is
	// ManagedResourceReconciler, which reads from this channel and
	// force-stamps every labeled ManagedResource on the hosting cluster
	// with the new caBundle, instead of waiting for its cache --sync-period
	// to fire.
	//
	// Nil when no MR reconciler is wired (e.g. --managed-resource-label
	// unset, or in tests that don't exercise rotation). Should be created
	// with a buffer of 1 so a missed send (because the receiver is mid-
	// resync) is harmless — the next rotation will fire again, and a single
	// signal is enough to trigger a full re-stamp pass.
	RotationTrigger chan<- struct{}

	// Gate is the shared rotation-gate snapshot. WebhookReconciler reads
	// it during the staged rotation phase to learn which CRDs on the
	// target cluster need their caBundle to show the union bundle before
	// the new serving cert is promoted. The set is populated by
	// ManagedResourceReconciler at stamp time — every CRD that
	// reconciler is currently responsible for is published, and the
	// union across all MRs becomes the gate.
	//
	// Nil disables the gate entirely (snapshot is always empty, gate
	// check returns propagated=true immediately, and the time-based
	// fallback via RotationGateTimeout is the only constraint). Pure-
	// ConfigMap deployments (no --managed-resource-label) leave this
	// nil, which is correct: they have no GRM hop to wait on; MWC/VWC
	// reach the target via synchronous Apply inside the same reconcile
	// pass.
	Gate *rotationgate.Gate

	// RotationOverlapWindow is the grace duration after the new cert is
	// promoted (active phase) before the old cert is pruned from the
	// trust bundle. Zero means "use defaultRotationOverlapWindow".
	// Set via --rotation-overlap-window.
	RotationOverlapWindow time.Duration

	// RotationGateTimeout caps how long the controller waits for shoot
	// CRDs to show the union bundle during the staged phase before
	// promoting anyway. Zero means "use defaultRotationGateTimeout".
	// Set via --rotation-gate-timeout.
	RotationGateTimeout time.Duration
}

// rotationOverlapWindow returns the configured overlap window or the
// package default if unset, so callers don't need to do the
// zero-value dance.
func (r *WebhookReconciler) rotationOverlapWindow() time.Duration {
	if r.RotationOverlapWindow > 0 {
		return r.RotationOverlapWindow
	}
	return defaultRotationOverlapWindow
}

// rotationGateTimeout returns the configured gate timeout or the package
// default if unset.
func (r *WebhookReconciler) rotationGateTimeout() time.Duration {
	if r.RotationGateTimeout > 0 {
		return r.RotationGateTimeout
	}
	return defaultRotationGateTimeout
}

// gateSnapshot returns the current rotation-gate CRD set. Nil-safe: if
// no Gate is wired (a deployment without --managed-resource-label, or
// a test that doesn't exercise rotation), it returns nil and
// checkPropagation falls through to the empty-gate fast path.
func (r *WebhookReconciler) gateSnapshot() []string {
	if r.Gate == nil {
		return nil
	}
	return r.Gate.Snapshot()
}

// Reconcile handles ConfigMap changes and certificate rotation
func (r *WebhookReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ConfigMap
	configMap := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, req.NamespacedName, configMap)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling webhook configuration", "configmap", req.NamespacedName)

	// Parse webhook configurations
	webhooksYAML, ok := configMap.Data["webhooks.yaml"]
	if !ok {
		return ctrl.Result{}, errors.New("webhooks.yaml not found in ConfigMap")
	}

	configs, err := webhooks.ParseWebhookConfigurations([]byte(webhooksYAML))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to parse webhook configurations: %w", err)
	}

	// Extract service references
	services := webhooks.ExtractServiceReferences(configs)
	// Append any user-supplied SANs (auto-classified DNS vs IP downstream).
	for _, san := range r.CertSANs {
		services = append(services, certificates.ServiceReference{Hostname: san})
	}
	if len(services) == 0 {
		logger.Info("No service references found in webhook configurations")
		return ctrl.Result{}, nil
	}

	// Check if certificates need generation/rotation
	secret, reason, err := r.checkCertificates(ctx, services)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check certificates: %w", err)
	}

	// Capture the rotation phase BEFORE we mutate anything. The dispatch
	// below uses both `reason` (why we're here) and `phase` (where the
	// state machine is) to pick an action.
	phase, _ := rotationPhase(secret)

	// Force-rotation gate: if a prior rotation cycle has not yet reached
	// steady-state active, refuse to start a new one. The dangerous shape
	// we're guarding against is `active + force` during the overlap window,
	// which would drop the previous old cert from `tls.crt` before its
	// in-flight TLS sessions and slow caBundle consumers had a chance to
	// migrate — i.e. real downtime. We deliberately apply the same gate to
	// `staged + force` for a single, uniform rule: force-rotation only ever
	// starts from a clean baseline. The annotation stays on the Secret so
	// the request is visibly pending; the next reconcile after the cycle
	// settles will pick it up. Time/SAN-driven rotations are NOT gated —
	// those are "we must rotate" signals and cannot legitimately fire mid-
	// rotation anyway (we just wrote a fresh cert).
	if reason == rotationReasonForced {
		if inFlight, remaining := isRotationInFlight(secret, r.rotationOverlapWindow()); inFlight {
			metrics.ForceRotationDeferredTotal.Inc()
			logger.Info("Force-rotation deferred: prior rotation still in flight",
				"phase", phase, "remaining", remaining)
			// Suppress the force reason so the staging block below is
			// skipped; the phase dispatch still runs so the in-flight
			// rotation keeps progressing. Requeue is handled by the phase
			// dispatch (staged → stagedRequeueInterval; active → remaining
			// overlap window); no extra requeue needed here.
			reason = ""
		}
	}

	// Rotation trigger fires when the leaf is missing/expired/SAN-mismatched
	// or the user set the force-rotation annotation. In every case we
	// (re)stage: append a fresh cert to the bundle and mark phase=staged.
	if reason != "" {
		logger.Info("Staging certificate rotation", "services", len(services), "reason", reason, "fromPhase", phase)
		if err := r.stageCertificate(ctx, services); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to stage certificate: %w", err)
		}
		metrics.CertificateRotationsTotal.WithLabelValues(string(reason)).Inc()

		// Re-fetch and signal downstream consumers so MR Secrets get the
		// new union caBundle stamped immediately. The signal is the same
		// regardless of phase transition shape.
		secret = &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      r.CertSecretName,
			Namespace: r.getNamespace(r.Namespace),
		}
		if err := r.Client.Get(ctx, secretKey, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to get generated secret: %w", err)
		}
		r.signalRotation(logger, reason)
		// Re-read the phase after the stage write — initial bootstrap goes
		// directly to active, every other path lands in staged.
		phase, _ = rotationPhase(secret)
	}

	// Phase-machine dispatch. The cases are independent of `reason`:
	// once we've staged (or skipped staging), the rest of the rotation
	// is a sequence of timed transitions driven by reconcile passes.
	switch phase {
	case PhaseStaged:
		if requeue, err := r.handleStagedPhase(ctx, secret); err != nil {
			return ctrl.Result{}, err
		} else if requeue > 0 {
			// We're waiting for shoot CRDs to show the union bundle.
			// Carry on through apply (to keep MWC/VWC fresh) and requeue
			// at the phase-machine cadence. If apply fails we still return
			// the requeue so the phase machine keeps ticking, but we also
			// surface the error so controller-runtime applies its own
			// backoff in parallel with our requeue (the larger of the two
			// wins).
			applyErr := r.applyAndUpdateExpiry(ctx, configs, secret, logger)
			return ctrl.Result{RequeueAfter: requeue}, applyErr
		}
		// Promoted in this pass: fall through to apply with the new key.
	case PhaseActive:
		if requeue, err := r.handleActivePhase(ctx, secret); err != nil {
			return ctrl.Result{}, err
		} else if requeue > 0 {
			applyErr := r.applyAndUpdateExpiry(ctx, configs, secret, logger)
			return ctrl.Result{RequeueAfter: requeue}, applyErr
		}
		// Either steady-state (no overlap to prune) or just pruned —
		// fall through to apply.
	}

	if err := r.applyAndUpdateExpiry(ctx, configs, secret, logger); err != nil {
		return ctrl.Result{}, err
	}
	logger.Info("Reconciliation complete")

	// Calculate next rotation time from certificate expiry
	nextRotation := r.calculateNextRotation(secret)
	if nextRotation > 0 {
		logger.Info("Scheduling next rotation check", "after", nextRotation)
		return ctrl.Result{RequeueAfter: nextRotation}, nil
	}

	// Fallback to sync period if we can't calculate rotation time
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// signalRotation pushes a non-blocking notification to RotationTrigger so
// ManagedResourceReconciler re-stamps every labeled MR with the new
// caBundle. Shared between the in-line rotation path (in Reconcile) and
// the phase-promotion/prune transitions which also change what gets
// stamped downstream.
func (r *WebhookReconciler) signalRotation(logger logr.Logger, reason rotationReason) {
	if r.RotationTrigger == nil {
		return
	}
	select {
	case r.RotationTrigger <- struct{}{}:
		logger.Info("Signaled rotation downstream consumers", "reason", reason)
	default:
		logger.V(1).Info("Rotation signal channel full, skipping (already pending)")
	}
}

// handleStagedPhase polls the target CRDs for caBundle propagation. If
// every gated CRD has both leaves visible (or the gate timeout has
// elapsed), it promotes — switching tls.key to the staged key. Returns
// a non-zero RequeueAfter if we should poll again later.
func (r *WebhookReconciler) handleStagedPhase(ctx context.Context, secret *corev1.Secret) (time.Duration, error) {
	logger := log.FromContext(ctx)
	_, startedAt := rotationPhase(secret)
	deadline := startedAt.Add(r.rotationGateTimeout())
	timeUp := !startedAt.IsZero() && time.Now().After(deadline)

	propagated, err := r.checkPropagation(ctx, secret)
	if err != nil {
		return 0, fmt.Errorf("propagation check: %w", err)
	}
	if !propagated && !timeUp {
		logger.V(1).Info("Staged: propagation not yet confirmed, will retry", "in", stagedRequeueInterval)
		return stagedRequeueInterval, nil
	}
	if timeUp && !propagated {
		logger.Info("Staged: gate timeout elapsed without confirmed propagation, promoting anyway",
			"timeout", r.rotationGateTimeout())
	}
	if err := r.promoteCertificate(ctx, secret); err != nil {
		return 0, fmt.Errorf("promote: %w", err)
	}
	r.signalRotation(logger, rotationReason("promote"))
	return 0, nil // promoted; let Reconcile fall through to apply.
}

// handleActivePhase prunes the old cert from the bundle once the overlap
// window has elapsed since promotion. While the window is open, returns
// a RequeueAfter equal to the remaining time. In steady state (no
// rotation annotations at all), returns 0 — nothing to do.
func (r *WebhookReconciler) handleActivePhase(ctx context.Context, secret *corev1.Secret) (time.Duration, error) {
	logger := log.FromContext(ctx)
	promotedAt := parseTimeAnnotation(secret.Annotations[rotationPromotedAtAnnotation])
	if promotedAt.IsZero() {
		// No promote marker => steady state (initial bootstrap or
		// already pruned). Nothing to do here.
		return 0, nil
	}
	pruneAt := promotedAt.Add(r.rotationOverlapWindow())
	if time.Now().Before(pruneAt) {
		remaining := time.Until(pruneAt)
		logger.V(1).Info("Active: waiting out overlap window before prune", "remaining", remaining)
		return remaining, nil
	}
	if err := r.pruneCertificate(ctx, secret); err != nil {
		return 0, fmt.Errorf("prune: %w", err)
	}
	r.signalRotation(logger, rotationReason("prune"))
	logger.Info("Pruned old cert from trust bundle; rotation cycle complete")
	return 0, nil
}

// parseTimeAnnotation returns the parsed time or zero if the input is
// empty / malformed. Callers treat zero as "annotation absent."
func parseTimeAnnotation(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// applyAndUpdateExpiry is the tail of every successful reconcile pass:
// inject the current caBundle into MWC/VWC, write them to the target
// cluster, and refresh the expiry gauge metric. Pulled out so all
// phase-machine code paths terminate identically.
//
// Returns the apply error so callers can surface it to controller-runtime
// for exponential backoff. The expiry gauge is refreshed even when the
// apply fails — the gauge reads from the cert Secret, not the target.
func (r *WebhookReconciler) applyAndUpdateExpiry(ctx context.Context, configs *webhooks.WebhookConfigurations, secret *corev1.Secret, logger logr.Logger) error {
	r.updateCertExpiryGauge(secret)
	caCert := secret.Data[tlsCrtKey]
	webhooks.InjectCABundle(configs, caCert)
	if err := r.applyWebhookConfigurations(ctx, configs); err != nil {
		logger.Error(err, "failed to apply webhook configurations")
		return fmt.Errorf("apply webhook configurations: %w", err)
	}
	return nil
}

// checkCertificates checks if certificates exist and are valid.
// Returns the rotation reason (empty string means no rotation needed).
func (r *WebhookReconciler) checkCertificates(ctx context.Context, services []certificates.ServiceReference) (*corev1.Secret, rotationReason, error) {
	secretKey := types.NamespacedName{
		Name:      r.CertSecretName,
		Namespace: r.getNamespace(r.Namespace),
	}

	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, secretKey, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, rotationReasonInitial, nil
		}
		return nil, "", err
	}

	// Check for force-rotation annotation
	if secret.Annotations != nil {
		if forceRotation, exists := secret.Annotations[ForceRotationAnnotation]; exists && forceRotation == annotationTrue {
			return secret, rotationReasonForced, nil
		}
	}

	// Check if certificate needs rotation based on time/SANs
	reason, err := r.needsCertificateRotation(secret, services)
	if err != nil {
		return nil, "", fmt.Errorf("failed to check certificate rotation: %w", err)
	}

	return secret, reason, nil
}

// leafFromSecret returns the leaf certificate (the one whose public key
// matches tls.key) from a cert Secret. During the steady/pruned state
// tls.crt has one block and tls.key matches it; during staged/active
// rotation tls.crt has two and the helper picks the one currently being
// served. Returns an error if the Secret is missing keys or the certs
// can't be parsed — callers treat this as "rotate from scratch."
func leafFromSecret(secret *corev1.Secret) (*x509.Certificate, error) {
	if secret == nil {
		return nil, errors.New("cert Secret is nil")
	}
	certPEM, ok := secret.Data[tlsCrtKey]
	if !ok || len(certPEM) == 0 {
		return nil, errors.New("tls.crt not found in secret")
	}
	keyPEM, ok := secret.Data[tlsKeyKey]
	if !ok || len(keyPEM) == 0 {
		return nil, errors.New("tls.key not found in secret")
	}
	return certificates.LeafCertificate(certPEM, keyPEM)
}

// rotationPhase reads the rotation phase from the Secret's annotations.
// An absent annotation means PhaseActive (steady state). Returns the
// started-at timestamp alongside; a zero time means "annotation absent
// or unparsable."
func rotationPhase(secret *corev1.Secret) (phase RotationPhase, startedAt time.Time) {
	phase = PhaseActive
	if secret == nil || secret.Annotations == nil {
		return phase, time.Time{}
	}
	if p := secret.Annotations[RotationPhaseAnnotation]; p != "" {
		phase = RotationPhase(p)
	}
	if ts := secret.Annotations[RotationStartedAtAnnotation]; ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			startedAt = parsed
		}
	}
	return phase, startedAt
}

// isRotationInFlight returns true when the given Secret is in the middle of
// a rotation cycle that has not yet settled to steady-state active — i.e.
// either phase=staged, or phase=active with the rotation-promoted-at marker
// still present (overlap window has not elapsed). Steady-state active (no
// promoted-at marker, no staged marker) and absent (initial bootstrap) both
// return false. The returned duration is an upper bound on how much longer
// the caller would have to wait before the cycle settles — used purely for
// log/visibility, not control flow (the phase dispatch already requeues at
// the correct cadence). A zero duration means "we can't compute a sensible
// bound" (annotation missing or unparsable) — treat as "wait at least one
// reconcile pass".
//
// This is the predicate behind the force-rotation gate: forcing a rotation
// while one is in flight would either drop a not-yet-trusted candidate
// (staged) or drop the previous cert before its overlap window expired
// (active+promoted-at), both of which the staged→active→pruned machine was
// designed to avoid.
func isRotationInFlight(secret *corev1.Secret, overlapWindow time.Duration) (inFlight bool, remaining time.Duration) {
	if secret == nil || secret.Annotations == nil {
		return false, 0
	}
	phase := RotationPhase(secret.Annotations[RotationPhaseAnnotation])
	switch phase {
	case PhaseStaged:
		// We don't know how long staged will last (depends on shoot CRD
		// propagation), but it's bounded by --rotation-gate-timeout. We
		// don't have access to that here, so return zero — callers log
		// it as "indeterminate, will resume on next reconcile".
		return true, 0
	case PhaseActive:
		promotedAt := parseTimeAnnotation(secret.Annotations[rotationPromotedAtAnnotation])
		if promotedAt.IsZero() {
			// Steady-state active (no overlap to wait out).
			return false, 0
		}
		pruneAt := promotedAt.Add(overlapWindow)
		if rem := time.Until(pruneAt); rem > 0 {
			return true, rem
		}
		// Overlap window already elapsed; next reconcile will prune.
		// Treat as in-flight one more pass so the prune happens first.
		return true, 0
	}
	return false, 0
}

// needsCertificateRotation checks if certificate needs rotation based on expiry and SANs.
// Returns the rotation reason or empty string if no rotation needed.
func (r *WebhookReconciler) needsCertificateRotation(secret *corev1.Secret, services []certificates.ServiceReference) (rotationReason, error) {
	cert, err := leafFromSecret(secret)
	if err != nil {
		return rotationReasonExpired, fmt.Errorf("failed to load leaf certificate: %w", err)
	}

	// Check expiry - rotate if past threshold
	now := time.Now()
	totalValidity := cert.NotAfter.Sub(cert.NotBefore)
	remainingValidity := cert.NotAfter.Sub(now)

	if remainingValidity <= 0 {
		return rotationReasonExpired, nil
	}

	// Calculate remaining percentage
	remainingPercent := float64(remainingValidity) / float64(totalValidity)

	// Rotate if below threshold
	if remainingPercent < r.CertRotationThreshold {
		return rotationReasonExpired, nil
	}

	// Check if SANs match current service references
	expectedDNS, expectedIPs := buildExpectedSANs(services)
	currentDNS := make(map[string]bool)
	for _, dnsName := range cert.DNSNames {
		currentDNS[dnsName] = true
	}
	currentIPs := make(map[string]bool)
	for _, ip := range cert.IPAddresses {
		currentIPs[ip.String()] = true
	}

	// Check if all expected DNS SANs are present
	for san := range expectedDNS {
		if !currentDNS[san] {
			return rotationReasonSANMismatch, nil
		}
	}
	// Check for extra DNS SANs
	for san := range currentDNS {
		if !expectedDNS[san] {
			return rotationReasonSANMismatch, nil
		}
	}

	// Check if all expected IP SANs are present
	for ip := range expectedIPs {
		if !currentIPs[ip] {
			return rotationReasonSANMismatch, nil
		}
	}
	// Check for extra IP SANs
	for ip := range currentIPs {
		if !expectedIPs[ip] {
			return rotationReasonSANMismatch, nil
		}
	}

	return "", nil
}

// buildExpectedSANs builds the expected sets of DNS SANs and IP SANs from service references
func buildExpectedSANs(services []certificates.ServiceReference) (dns, ips map[string]bool) {
	dns = make(map[string]bool)
	ips = make(map[string]bool)
	for _, svc := range services {
		if svc.Hostname != "" {
			if ip := net.ParseIP(svc.Hostname); ip != nil {
				ips[ip.String()] = true
			} else {
				dns[svc.Hostname] = true
			}
		} else if svc.Name != "" && svc.Namespace != "" {
			dns[fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace)] = true
			dns[fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)] = true
		}
	}
	return dns, ips
}

// calculateNextRotation calculates when the next certificate rotation check should occur
// based on the LEAF certificate's expiry time and the configured rotation threshold.
// Returns the duration until the next check, or 0 if the certificate info can't be parsed.
func (r *WebhookReconciler) calculateNextRotation(secret *corev1.Secret) time.Duration {
	cert, err := leafFromSecret(secret)
	if err != nil {
		return 0
	}

	// Calculate when the certificate will reach the rotation threshold
	now := time.Now()
	totalValidity := cert.NotAfter.Sub(cert.NotBefore)

	// Time when rotation should happen: when remaining validity drops below threshold
	rotationTime := cert.NotBefore.Add(time.Duration(float64(totalValidity) * (1 - r.CertRotationThreshold)))

	// Calculate duration until rotation time
	untilRotation := rotationTime.Sub(now)

	// If rotation time has passed or is very soon, return a short duration
	if untilRotation <= 0 {
		return time.Second
	}

	// Add a small buffer (1 second) to ensure we don't miss the rotation window
	return untilRotation + time.Second
}

// stageCertificate handles the active→staged (and staged→staged re-stage)
// transition: generate a fresh cert, append it to tls.crt as a
// concatenated PEM bundle, stash the new private key under
// tls.staged.key, and mark phase=staged with started=now.
//
// If the existing Secret is in steady state (no rotation annotations,
// single cert in tls.crt), the result is tls.crt = old||new, tls.key = old.
// If the existing Secret is already staged (re-stage triggered by another
// force-rotation), we replace the staged candidate with a fresh one:
// tls.crt = leaf||new, tls.key = leaf (the unpromoted candidate is
// dropped, no need to keep it).
//
// Initial bootstrap (no existing Secret) writes a single-cert Secret
// directly in active phase — there's no "old" to overlap with.
//
// Annotations OTHER than the controller's own (ForceRotation, Phase,
// StartedAt) are preserved across writes, so external tooling can label
// the cert Secret without it being clobbered on every rotation.
func (r *WebhookReconciler) stageCertificate(ctx context.Context, services []certificates.ServiceReference) error {
	logger := log.FromContext(ctx)

	newCertPEM, newKeyPEM, err := certificates.GenerateSelfSignedCert(services, r.CertValidity)
	if err != nil {
		return fmt.Errorf("generate certificate: %w", err)
	}

	secretKey := types.NamespacedName{
		Name:      r.CertSecretName,
		Namespace: r.getNamespace(r.Namespace),
	}
	existing := &corev1.Secret{}
	if err := r.Client.Get(ctx, secretKey, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get cert Secret: %w", err)
		}
		// Initial bootstrap: write a single-cert active-phase Secret.
		// No incoming Secret, so no force annotation to clear; the
		// preservedAnnotations(nil) return is empty.
		bootstrap := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        secretKey.Name,
				Namespace:   secretKey.Namespace,
				Annotations: preservedAnnotations(nil),
			},
			Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{
				tlsCrtKey: newCertPEM,
				tlsKeyKey: newKeyPEM,
			},
		}
		metrics.PhaseTransitionsTotal.WithLabelValues("absent", string(PhaseActive)).Inc()
		return r.Client.Create(ctx, bootstrap)
	}

	// We have an existing Secret. Determine the current leaf cert so we
	// can pair it with the new cert in the bundle. If the existing
	// Secret is unparsable (corrupt or never-populated), fall back to a
	// single-cert active-phase write — there's nothing useful to keep.
	leaf, err := leafFromSecret(existing)
	if err != nil {
		logger.Info("existing cert Secret unreadable; re-bootstrapping single-cert active phase", "reason", err.Error())
		fromPhase, _ := rotationPhase(existing)
		existing.Type = corev1.SecretTypeTLS
		existing.Data = map[string][]byte{
			tlsCrtKey: newCertPEM,
			tlsKeyKey: newKeyPEM,
		}
		existing.Annotations = preservedAnnotations(existing.Annotations)
		// Consume the force annotation: stageCertificate is the rotation
		// entry point, so once we stage we've honoured the request.
		delete(existing.Annotations, ForceRotationAnnotation)
		metrics.PhaseTransitionsTotal.WithLabelValues(string(fromPhase), string(PhaseActive)).Inc()
		return r.Client.Update(ctx, existing)
	}

	// Append the new cert to the leaf's PEM. EncodeChain([leaf]) is the
	// canonical re-encoding of just the leaf (drops any unpromoted
	// candidate if we're re-staging from a staged phase). We append the
	// raw newCertPEM bytes — GenerateSelfSignedCert already emits a
	// well-formed PEM block, and concatenation is the inverse of
	// ParseChain at the byte level.
	bundle := append(certificates.EncodeChain([]*x509.Certificate{leaf}), newCertPEM...)

	fromPhase, _ := rotationPhase(existing)
	existing.Type = corev1.SecretTypeTLS
	existing.Data = map[string][]byte{
		tlsCrtKey:       bundle,
		tlsKeyKey:       existing.Data[tlsKeyKey], // keep serving the existing leaf
		tlsStagedKeyKey: newKeyPEM,
	}
	annotations := preservedAnnotations(existing.Annotations)
	// Consume the force annotation: the rotation it requested is starting
	// right now. preservedAnnotations no longer strips this for us so
	// promote/prune can preserve a deferred force, but the normal stage
	// path always consumes it.
	delete(annotations, ForceRotationAnnotation)
	annotations[RotationPhaseAnnotation] = string(PhaseStaged)
	annotations[RotationStartedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
	existing.Annotations = annotations

	metrics.PhaseTransitionsTotal.WithLabelValues(string(fromPhase), string(PhaseStaged)).Inc()
	return r.Client.Update(ctx, existing)
}

// promoteCertificate handles the staged→active transition: swap tls.key
// to the staged key, drop tls.staged.key from the Secret, reorder
// tls.crt so the new cert (the LEAF being served) is first in the
// bundle, mark phase=active, and stamp a promoted-at marker so the
// active→pruned timer is anchored on the promotion moment.
//
// Why we reorder: `tls.LoadX509KeyPair` (used by controller-runtime's
// certwatcher and most TLS stacks) reads the FIRST CERTIFICATE block in
// the cert file as the leaf and pairs it with the key. If the bundle
// stays `old||new` after we swap tls.key to the new key, that pairing
// fails (old cert + new key = "private key does not match public key").
// We rewrite the bundle as `new||old` at promote time — both still trusted
// on the apiserver side because the caBundle carries both — but the
// webhook server now serves the new cert.
func (r *WebhookReconciler) promoteCertificate(ctx context.Context, secret *corev1.Secret) error {
	stagedKey, ok := secret.Data[tlsStagedKeyKey]
	if !ok || len(stagedKey) == 0 {
		return errors.New("promote: tls.staged.key missing from Secret")
	}

	// Locate the new leaf in tls.crt by matching public keys against
	// the staged private key. The old cert is whatever ISN'T the new
	// leaf — there should be exactly two certs at this point.
	certs, err := certificates.ParseChain(secret.Data[tlsCrtKey])
	if err != nil {
		return fmt.Errorf("promote: parse tls.crt: %w", err)
	}
	newLeaf, err := certificates.LeafCertificate(secret.Data[tlsCrtKey], stagedKey)
	if err != nil {
		return fmt.Errorf("promote: cannot find new leaf in bundle: %w", err)
	}
	// Reorder: new leaf first, everything else after. Preserves the OLD
	// cert in the bundle so the apiserver's caBundle (which we built
	// from this same tls.crt) still trusts both.
	ordered := []*x509.Certificate{newLeaf}
	for _, c := range certs {
		if c.Equal(newLeaf) {
			continue
		}
		ordered = append(ordered, c)
	}

	secret.Data[tlsCrtKey] = certificates.EncodeChain(ordered)
	secret.Data[tlsKeyKey] = stagedKey
	delete(secret.Data, tlsStagedKeyKey)

	annotations := preservedAnnotations(secret.Annotations)
	annotations[RotationPhaseAnnotation] = string(PhaseActive)
	// Preserve started-at as the timestamp the rotation began, NOT
	// the moment of promotion. The prune deadline math is anchored on
	// the new promoted-at marker (see rotationPromotedAtAnnotation).
	if existing := secret.Annotations[RotationStartedAtAnnotation]; existing != "" {
		annotations[RotationStartedAtAnnotation] = existing
	} else {
		annotations[RotationStartedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
	}
	annotations[rotationPromotedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)
	secret.Annotations = annotations
	metrics.PhaseTransitionsTotal.WithLabelValues(string(PhaseStaged), string(PhaseActive)).Inc()
	return r.Client.Update(ctx, secret)
}

// pruneCertificate handles the active(bundle)→active(single) transition:
// strip the old cert from tls.crt, leaving only the leaf. Removes all
// rotation annotations so future rotations start from a clean slate.
func (r *WebhookReconciler) pruneCertificate(ctx context.Context, secret *corev1.Secret) error {
	leaf, err := leafFromSecret(secret)
	if err != nil {
		return fmt.Errorf("prune: cannot identify leaf cert: %w", err)
	}
	secret.Data[tlsCrtKey] = certificates.EncodeChain([]*x509.Certificate{leaf})
	delete(secret.Data, tlsStagedKeyKey) // belt-and-suspenders: nothing should be there
	secret.Annotations = preservedAnnotations(secret.Annotations)
	metrics.PhaseTransitionsTotal.WithLabelValues(string(PhaseActive), "pruned").Inc()
	return r.Client.Update(ctx, secret)
}

// preservedAnnotations returns a fresh map containing every annotation
// from src EXCEPT the controller-owned rotation-phase annotations (which
// the caller will set explicitly if it wants them, and otherwise clears).
// The ForceRotation annotation is NOT stripped here — it has consume-on-
// rotation semantics that only stageCertificate is in a position to
// enforce. Promote/prune passes that happen while a force-rotation is
// deferred (gated by isRotationInFlight) must NOT swallow the pending
// request, so they call this helper which leaves the annotation alone.
func preservedAnnotations(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		switch k {
		case RotationPhaseAnnotation,
			RotationStartedAtAnnotation,
			rotationPromotedAtAnnotation:
			continue
		}
		out[k] = v
	}
	return out
}

// rotationPromotedAtAnnotation is package-private: it exists to anchor
// the active→pruned timer at the moment of promotion (not at rotation
// start, which is what RotationStartedAtAnnotation captures). Operators
// shouldn't need to see it.
const rotationPromotedAtAnnotation = "webhook-injector.sap.com/rotation-promoted-at"

// checkPropagation returns true if every CRD published on the shared
// rotation Gate has BOTH the current leaf and the staged cert in
// its conversion webhook caBundle. The gate set is derived at call
// time from the shared rotationgate.Gate snapshot — every CRD that
// ManagedResourceReconciler has published is gated automatically. An
// empty snapshot (no Gate wired, or no MR reconciler running, or no
// labeled MRs shipping CRDs yet) is treated as "no gate"; we return
// true immediately and let the staged→active transition happen on the
// next reconcile. The time-based fallback via --rotation-gate-timeout
// then applies if the caller still wants to wait.
//
// Errors fetching individual CRDs (NotFound, Forbidden) are logged and
// treated as "not propagated yet" so they don't poison the gate check.
// The caller falls back to the time-based gate timeout if propagation
// can't be confirmed.
func (r *WebhookReconciler) checkPropagation(ctx context.Context, secret *corev1.Secret) (bool, error) {
	gateCRDs := r.gateSnapshot()
	if len(gateCRDs) == 0 {
		return true, nil
	}
	logger := log.FromContext(ctx)

	// What two certs do we expect to find on every gated CRD's caBundle?
	bundlePEM, ok := secret.Data[tlsCrtKey]
	if !ok {
		return false, errors.New("cert Secret has no tls.crt")
	}
	expected, err := certificates.ParseChain(bundlePEM)
	if err != nil {
		return false, fmt.Errorf("parse staged bundle: %w", err)
	}
	if len(expected) < 2 {
		// Not actually a bundle — either still single-cert or already
		// pruned. Nothing to gate on; treat as propagated.
		return true, nil
	}
	expectedFPs := make(map[[32]byte]struct{}, len(expected))
	for _, c := range expected {
		expectedFPs[fingerprint(c)] = struct{}{}
	}

	if r.TargetClient == nil {
		// No target client wired — can't gate. Time-based fallback only.
		logger.V(1).Info("checkPropagation: TargetClient nil, skipping CRD poll")
		return false, nil
	}

	for _, name := range gateCRDs {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := r.TargetClient.Get(ctx, types.NamespacedName{Name: name}, crd); err != nil {
			logger.V(1).Info("checkPropagation: target CRD not (yet) readable",
				"crd", name, "err", err.Error())
			return false, nil
		}
		if crd.Spec.Conversion == nil ||
			crd.Spec.Conversion.Webhook == nil ||
			crd.Spec.Conversion.Webhook.ClientConfig == nil {
			logger.V(1).Info("checkPropagation: target CRD has no conversion webhook clientConfig", "crd", name)
			return false, nil
		}
		got, err := certificates.ParseChain(crd.Spec.Conversion.Webhook.ClientConfig.CABundle)
		if err != nil {
			logger.V(1).Info("checkPropagation: target CRD caBundle unparsable", "crd", name, "err", err.Error())
			return false, nil
		}
		gotFPs := make(map[[32]byte]struct{}, len(got))
		for _, c := range got {
			gotFPs[fingerprint(c)] = struct{}{}
		}
		for fp := range expectedFPs {
			if _, ok := gotFPs[fp]; !ok {
				logger.V(1).Info("checkPropagation: target CRD caBundle missing expected cert", "crd", name)
				return false, nil
			}
		}
	}
	return true, nil
}

// fingerprint returns a SHA-256 of the cert's DER bytes — stable across
// PEM encodings, suitable as a map key for equality checks.
func fingerprint(cert *x509.Certificate) [32]byte {
	return sha256.Sum256(cert.Raw)
}

// applyWebhookConfigurations applies webhook configs to target cluster.
//
// Increments TargetStampsTotal per Create/Update/error outcome so the
// caBundle-propagation alerts (WebhookTargetStampErrors etc. in
// controlplane-operations) have a live signal to fire on. The
// "converged" no-op result is not distinguished here — every reconcile
// pass Updates regardless of equality, so an idempotent reconcile shows
// up as "stamped" rather than "converged". Adding equality detection
// here would let the counter distinguish "we changed something" from
// "we wrote the same bytes" but is out of scope for the rotation-bug
// review.
func (r *WebhookReconciler) applyWebhookConfigurations(ctx context.Context, configs *webhooks.WebhookConfigurations) error {
	logger := log.FromContext(ctx)
	const (
		kindMWC = "MutatingWebhookConfiguration"
		kindVWC = "ValidatingWebhookConfiguration"
	)

	// Apply mutating webhook configurations
	for _, mwc := range configs.Mutating {
		logger.Info("Applying MutatingWebhookConfiguration", "name", mwc.Name)

		existing := &admissionv1.MutatingWebhookConfiguration{}
		err := r.TargetClient.Get(ctx, types.NamespacedName{Name: mwc.Name}, existing)

		if err != nil {
			if apierrors.IsNotFound(err) {
				// Create new
				if err := r.TargetClient.Create(ctx, mwc); err != nil {
					metrics.TargetStampsTotal.WithLabelValues(kindMWC, metrics.ResultError).Inc()
					return fmt.Errorf("failed to create MutatingWebhookConfiguration %s: %w", mwc.Name, err)
				}
				metrics.TargetStampsTotal.WithLabelValues(kindMWC, metrics.ResultStamped).Inc()
				logger.Info("Created MutatingWebhookConfiguration", "name", mwc.Name)
			} else {
				metrics.TargetStampsTotal.WithLabelValues(kindMWC, metrics.ResultError).Inc()
				return fmt.Errorf("failed to get MutatingWebhookConfiguration %s: %w", mwc.Name, err)
			}
		} else {
			// Update existing
			existing.Webhooks = mwc.Webhooks
			if err := r.TargetClient.Update(ctx, existing); err != nil {
				metrics.TargetStampsTotal.WithLabelValues(kindMWC, metrics.ResultError).Inc()
				return fmt.Errorf("failed to update MutatingWebhookConfiguration %s: %w", mwc.Name, err)
			}
			metrics.TargetStampsTotal.WithLabelValues(kindMWC, metrics.ResultStamped).Inc()
			logger.Info("Updated MutatingWebhookConfiguration", "name", mwc.Name)
		}
	}

	// Apply validating webhook configurations
	for _, vwc := range configs.Validating {
		logger.Info("Applying ValidatingWebhookConfiguration", "name", vwc.Name)

		existing := &admissionv1.ValidatingWebhookConfiguration{}
		err := r.TargetClient.Get(ctx, types.NamespacedName{Name: vwc.Name}, existing)

		if err != nil {
			if apierrors.IsNotFound(err) {
				// Create new
				if err := r.TargetClient.Create(ctx, vwc); err != nil {
					metrics.TargetStampsTotal.WithLabelValues(kindVWC, metrics.ResultError).Inc()
					return fmt.Errorf("failed to create ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
				}
				metrics.TargetStampsTotal.WithLabelValues(kindVWC, metrics.ResultStamped).Inc()
				logger.Info("Created ValidatingWebhookConfiguration", "name", vwc.Name)
			} else {
				metrics.TargetStampsTotal.WithLabelValues(kindVWC, metrics.ResultError).Inc()
				return fmt.Errorf("failed to get ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
			}
		} else {
			// Update existing
			existing.Webhooks = vwc.Webhooks
			if err := r.TargetClient.Update(ctx, existing); err != nil {
				metrics.TargetStampsTotal.WithLabelValues(kindVWC, metrics.ResultError).Inc()
				return fmt.Errorf("failed to update ValidatingWebhookConfiguration %s: %w", vwc.Name, err)
			}
			metrics.TargetStampsTotal.WithLabelValues(kindVWC, metrics.ResultStamped).Inc()
			logger.Info("Updated ValidatingWebhookConfiguration", "name", vwc.Name)
		}
	}

	return nil
}

// updateCertExpiryGauge sets the certificate expiry metric from the secret's LEAF cert.
func (r *WebhookReconciler) updateCertExpiryGauge(secret *corev1.Secret) {
	cert, err := leafFromSecret(secret)
	if err != nil {
		return
	}
	metrics.CertificateExpiryTime.Set(float64(cert.NotAfter.Unix()))
}

// getNamespace returns the namespace, using pod namespace as fallback
func (r *WebhookReconciler) getNamespace(namespace string) string {
	if namespace != "" {
		return namespace
	}
	// Try to read from pod
	if ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return string(ns)
	}
	return defaultNamespace
}

// SetupWithManager sets up the controller with the Manager
func (r *WebhookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only watch the specific webhook ConfigMap
	configMapPredicate := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return r.WebhookConfigName == object.GetName() && r.getNamespace(r.Namespace) == object.GetNamespace()
	})

	// Map Secret events to ConfigMap reconcile requests
	// When the Secret changes (e.g., rotation annotation added), we reconcile the ConfigMap
	// IMPORTANT: Filter inside the mapper since WithEventFilter doesn't apply to secondary watches
	secretToConfigMapMapper := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		// Only map events for the specific certificate Secret
		if obj.GetName() != r.CertSecretName ||
			obj.GetNamespace() != r.getNamespace(r.Namespace) {
			return nil
		}

		return []ctrl.Request{
			{
				NamespacedName: types.NamespacedName{
					Name:      r.WebhookConfigName,
					Namespace: r.getNamespace(r.Namespace),
				},
			},
		}
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(configMapPredicate)).
		// Watch the certificate Secret for annotation-based rotation triggers
		Watches(&corev1.Secret{}, secretToConfigMapMapper).
		Complete(r)
}
