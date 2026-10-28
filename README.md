# WMS Platform

**A modular, on-premises Warehouse Management System backend — with a reproducible two-VM Hyper-V deployment lab.**

Go · PostgreSQL · OIDC · Hyper-V · PowerShell · Python · Ubuntu · Docker · Prometheus · Grafana

> **Project status:** Engineering prototype / portfolio project. The backend, deployment automation, and selected operational scenarios are implemented and tested. There is **no finished end-user WMS interface**, and the project is **not certified or warranted for production use**.

## See the infrastructure working

**[Hyper-V portfolio: screenshots, a 36-second demonstration, and verification notes](docs/portfolio/hyperv-lab/README.md)**

![WMS-Lab-A monitoring after recovery](docs/portfolio/hyperv-lab/05-wms-a-recovered.jpg)

The lab demonstrates two **separately deployed WMS installations** on two Ubuntu 24.04 virtual machines hosted by Hyper-V on **one physical Windows PC**:

| Area | What is implemented / demonstrated |
| --- | --- |
| Virtualization | Two Generation 2 VMs, separate VHDX disks, VM ownership checks, existing Hyper-V virtual switch |
| Provisioning | PowerShell and Python automation, cloud-init / NoCloud seed media, independent SSH keys, host-key pinning |
| Deployment | Per-instance release installer, systemd-managed Go API, PostgreSQL, Keycloak, Caddy, TLS |
| Observability | Prometheus metrics, Grafana dashboards, availability/readiness checks |
| Fault handling | Controlled interruption of installation A, continued operation of B, and recovery of A |
| Verification | Application smoke/replay checks, credential-isolation probes, recovery evidence, automated CI regressions |

**Evidence:** [lab notes and screenshots](docs/portfolio/hyperv-lab/README.md) · [recorded demo](docs/portfolio/hyperv-lab/wms-two-instances-demo.mp4) · [tested commit](https://github.com/Shpuntyara/wms-platform/commit/89fe54add10e8f3094810fb4da22438ecb5fac5a) · [successful CI run](https://github.com/Shpuntyara/wms-platform/actions/runs/40729659428).

**Scope:** This is a local lab, **not** a highly available cluster: both VMs share one physical host, backups are local, and the edited demo has cuts between its three 12-second segments. The public results prove the documented scenarios, not production uptime or full disaster recovery.

## What the backend does

The application implements a transaction-oriented warehouse accounting core with extension contracts for customer-specific workflows.

- **Stock and reservations:** receiving, reservation, reservation release, transfers, picking, and shipping operations with accounting invariants.
- **Warehouse documents:** API lifecycle for receiving, put-away, reservation, picking, shipping, and closure.
- **Identity and access:** OIDC access-token validation, organization/warehouse/owner scoping, and separated API/worker database privileges.
- **Extensibility:** versioned module contracts and a compiled `dispatch-quality` extension; the core retains authority over inventory movements.
- **Reliability:** idempotent commands, checked database migrations, transactional outbox, background workers, monitoring, and explicit failure paths.
- **Testing:** Go unit/integration/race tests, PostgreSQL checks, adversarial security tests, and failure/recovery exercises.

For architecture and boundaries, see [on-premises design](docs/on-premise.md), [Go backend](docs/go-backend.md), [PostgreSQL core](docs/postgresql-core.md), and [warehouse workflow](docs/warehouse-workflow.md).

## Deployment model

Each customer receives **an independent WMS installation, database, and server**. An installation may serve multiple organizations and warehouses under its own access rules. The platform does **not** implement a shared SaaS tenant database.

```text
Hyper-V host (lab)
├── WMS-Lab-A (Ubuntu 24.04, independent keys/storage)
│   ├── Caddy HTTPS → Go API + background worker (systemd)
│   ├── Keycloak / OIDC
│   ├── PostgreSQL
│   └── Prometheus → Grafana
└── WMS-Lab-B (same isolated service composition)
```

A deployment uses the documented Linux installer rather than an unrestricted host setup script. The installers and tests deliberately include guards for ownership, secret handling, archive integrity, preflight validation, upgrades, and restoration.

- [Hyper-V lab setup and verification](docs/hyperv-lab.md)
- [Linux deployment installer](docs/deployment-installer.md)
- [Security hardening](docs/security-hardening.md)
- [PostgreSQL recovery scenario](docs/postgres-resilience.md)

## Build and verification

This repository currently specifies **Go 1.27.3** for its backend. After you have provisioned a separate development PostgreSQL database and a valid OIDC configuration:

```bash
go build -o work/wms-api ./cmd/wms-api
```

The API defaults to `127.0.0.1:8080`. **Database migrations are not applied automatically on API startup.** A fresh installation requires the schema, role grants, `onprem-v1` profile, OIDC configuration, and independently generated runtime secrets described in the deployment documentation. Do not treat `.env.example` as a working credential file.

Primary entry points:

| Resource | Purpose |
| --- | --- |
| [OpenAPI 3.1](api/openapi.json) | HTTP interface contract |
| [SQL migrations](db/migrations/) | Versioned schema and warehouse operations |
| [Module contracts](contracts/) | Customer extension APIs and schemas |
| [Go integration guide](docs/go-backend.md) | Auth, API setup, modules, and tests |
| [Tests](tests/README.md) | Test setup and supported verification cases |
| [CI workflow](.github/workflows/security.yml) | Automated correctness/security checks |
| [Core test result](docs/verification/postgres-kernel.json) | Recorded PostgreSQL kernel verification |
| [Security result](docs/verification/security-green.json) | Recorded security checks |

### Releases and compatibility

The documented core/SDK baseline is **core 0.4.0 / SDK 2.0.0**, with subsequent **0.5.0 background/security** and **0.6.0 warehouse-workflow** work. The example `dispatch-quality` module is at **1.2.0**. Version strings embedded in example contracts are illustrative; they do not imply externally published production releases. See the [backend audit](docs/reviews/2026-10-27-backend-audit.md) and [load-test report](docs/reports/2026-10-27-load.md).

## Limitations and responsible disclosure

This repository is an engineering portfolio, not a turnkey system. The user interface, customer integrations, operational SLAs, third-party security review, and production deployment guarantees are out of scope or incomplete. Some linked documentation and test evidence remain in Russian.

**No real passwords, private keys, access tokens, client data, or production backups should be committed.** Use unique per-installation secrets and keep protected runtime evidence outside Git. Before public release, scan the **entire Git history**, review screenshots/video for visible confidential information, and rotate any credentials that might have been committed, even if later removed.

**Licensing:** A commercial/product license has not yet been selected. Do not assume rights to redistribute or deploy this project commercially.
