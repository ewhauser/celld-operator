# Architecture decision records

These records capture the celld operator decisions agreed on 18 September 2026. **Accepted** means the project direction is agreed, not that the behavior has been implemented or qualified. **Proposed** identifies architecture from the original design that has not been separately confirmed.

| Record | Decision | Status |
| --- | --- | --- |
| [0001](0001-controller-architecture.md) | Go operator with separate capacity policy and lifecycle reconciliation | Proposed |
| [0002](0002-durability-and-scaling.md) | Support both durability modes and automatic scaling in both directions | Accepted |
| [0003](0003-aws-platform-and-provisioning.md) | Target EKS, S3, and EBS; keep AWS and node provisioning external | Accepted |
| [0004](0004-availability-zone-placement.md) | Configurable AZ count with strict placement by default | Accepted |
| [0005](0005-application-disruption.md) | Permit Cloudflare-style request and connection interruptions | Accepted |
| [0006](0006-metrics-dependencies.md) | Keep Prometheus optional | Accepted |
| [0007](0007-runtime-compatibility.md) | Use an existing, unmodified celld release | Accepted |
| [0008](0008-recovery-evidence-and-conservative-removal.md) | Require read-only S3 evidence for peer-disk automatic contraction | Accepted |
| [0009](0009-service-and-ingress-boundary.md) | Expose ClusterIP Services; keep ingress, TLS, and DNS external | Accepted |
| [0011](0011-initial-fleet-api.md) | Experimental API, reservations and initial provisioning gate | Implemented locally |
| [0010](0010-production-qualification.md) | Qualify runtime-dependent behavior before production enablement | Accepted |

Read [runtime qualification](../runtime-qualification.md), [shutdown evidence](../shutdown-evidence.md), and [S3 recovery evidence](../s3-recovery-evidence.md) for implementation evidence and unresolved gates. The later S3 investigation corrects the earlier interpretation of sealed logs: sealing alone does not establish lossless recovery.

The original proposal is `/Users/ewhauser/Downloads/Celld_Fleet_Operator_Design.docx`. This directory is the portable record of the decisions; the source document is not required to understand them.

Future decisions should receive a new numbered record. Material changes should identify which earlier record they supersede. Do not rewrite a qualification hypothesis as a proven guarantee.

- [ADR 0012: restart-safe manual lifecycle](0012-restart-safe-manual-lifecycle.md)

Step 4: [ADR 0013: capacity collection and policy](0013-capacity-policy.md).

Step 5: [ADR 0014: coordinated maintenance and retained deletion](0014-coordinated-maintenance.md).
