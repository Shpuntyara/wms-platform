# WMS: Hyper-V lab portfolio

Evidence from **28 October 2026**, documenting two independent on-premises WMS installations on Ubuntu VMs hosted by Hyper-V. The screenshots show an actual Ubuntu guest and Grafana dashboards using **synthetic data**.

**Verified release:** [`89fe54a`](https://github.com/Shpuntyara/wms-platform/commit/89fe54add10e8f3094810fb4da22438ecb5fac5a) · [successful CI run](https://github.com/Shpuntyara/wms-platform/actions/runs/40729659428)

## Short demonstration

[Watch the 36-second two-instance demo](wms-two-instances-demo.mp4). It contains three edited 12-second segments: installation A stopped, installation B remaining operational, and installation A recovered. There are cuts between segments; this is **not** a continuous uptime recording.

## Screenshots

| File | What it shows |
| --- | --- |
| [01 — Ubuntu console](01-ubuntu-hyperv-console.jpg) | Booted Ubuntu guest inside Hyper-V |
| [02 — A initial state](02-wms-a-live-grafana.jpg) | Grafana dashboard for installation A |
| [03 — B live](03-wms-b-live-grafana.jpg) | Separately deployed installation B |
| [04 — A outage](04-wms-a-controlled-outage.jpg) | Controlled API outage on A, readiness = 0 |
| [05 — A recovered](05-wms-a-recovered.jpg) | API restored on A, readiness = 1 |
| [06 — report](06-lab-report-preview.jpg) | Summary of lab verification results |

![Installation A monitoring after recovery](05-wms-a-recovered.jpg)

SHA-256 digests for the media files: [SHA256SUMS](SHA256SUMS).

## What this does—and does not—prove

Both VMs run on **one physical PC** and have independent deployment identities, storage, and service configuration. The observed incident illustrates service-level isolation between two installations; it is **not** evidence of physical-host fault tolerance, clustering, or an independently audited disaster-recovery system. Backups remain local in this lab. All published demo data is synthetic.

Technical methodology: [Hyper-V bootstrap](../../hyperv-lab.md) · [Linux installer and operations](../../deployment-installer.md).
