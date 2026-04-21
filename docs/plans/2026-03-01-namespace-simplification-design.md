<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# Namespace Configuration Simplification

**Date:** 2026-03-01
**Status:** Approved

## Problem

The current namespace configuration is unnecessarily complex:

- Requires 3 separate environment variables: `WEBHOOK_CONFIG_NAMESPACE`, `CERT_SECRET_NAMESPACE`, `LEADER_ELECTION_NAMESPACE`
- Config struct has 3 separate fields that always have the same value
- Namespace auto-detection logic is scattered in `main.go`'s `getPodNamespace()` function
- Deployment manifests have repetitive boilerplate

This violates YAGNI—all resources must be in the same namespace, so having 3 separate configuration points adds no value.

## Solution

Replace the 3 namespace environment variables with a single `NAMESPACE` environment variable. Move namespace resolution into config initialization so it happens once at startup.

### Changes

**Configuration:**
- Replace 3 env vars with single `NAMESPACE` env var
- Replace 3 Config fields with single `Namespace` field
- Move namespace auto-detection into `config.LoadFromEnv()`
- Resolution order:
  1. `NAMESPACE` env var if set
  2. Read `/var/run/secrets/kubernetes.io/serviceaccount/namespace`
  3. Default to `"default"`

**Code structure:**
- Remove `getPodNamespace()` from `main.go`
- Update controllers to use `cfg.Namespace` instead of separate fields
- Update all tests to use single namespace field

**Deployment manifests:**
- Simplify to single namespace env var using `fieldRef: metadata.namespace`

## Benefits

- **Simpler configuration:** 1 env var instead of 3
- **Cleaner code:** Namespace resolution happens once at startup
- **Less boilerplate:** Deployment manifests are shorter
- **More honest design:** Config struct reflects reality (one namespace, not three)
- **Better testability:** Single source of truth for namespace

## Trade-offs

- **Breaking change:** Existing deployments must update their env vars
  - Acceptable: Project is early in lifecycle, clean break is better than technical debt
- **Multiple files to change:** Touches config, main, controllers, manifests, tests
  - Acceptable: Changes are straightforward search-and-replace

## Implementation Files

- `pkg/config/config.go` - Replace 3 fields with 1, add auto-detection to LoadFromEnv()
- `cmd/webhook-injector/main.go` - Remove getPodNamespace(), update logging, use cfg.Namespace for cache
- `pkg/controllers/reconciler.go` - Change cfg.WebhookConfigNamespace, cfg.CertSecretNamespace to cfg.Namespace
- `pkg/controllers/reloader.go` - Change cfg.CertSecretNamespace to cfg.Namespace
- `config/deployment.yaml` - Replace 3 namespace env vars with 1
- `test/e2e/manifests/webhook-server-deployment.yaml` - Replace 3 namespace env vars with 1
- `pkg/config/config_test.go` - Update test cases
- `pkg/controllers/reconciler_test.go` - Update config creation
- `pkg/controllers/reloader_test.go` - Update config creation

## Testing

- Unit tests verify namespace resolution order (env var → serviceaccount → default)
- Controller tests use simplified config creation
- E2E tests verify behavior with single NAMESPACE env var
- Verify all namespace references use cfg.Namespace consistently
