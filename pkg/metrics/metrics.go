// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
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
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		CertificateRotationsTotal,
		CertificateExpiryTime,
		CertFilesWrittenTotal,
	)
}
