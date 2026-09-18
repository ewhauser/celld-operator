# Celld operator documentation

Start with the [architecture decision records](decisions/README.md) for accepted scope, operating boundaries, and the proposed controller architecture.

The supporting investigations are:

- [Runtime qualification](runtime-qualification.md): released capabilities, startup behavior, persistent identity, and qualification gaps.
- [Shutdown evidence](shutdown-evidence.md): why successful process exit and HTTP status do not prove peer-log recovery completion.
- [Read-only S3 recovery evidence](s3-recovery-evidence.md): the proposed metadata contract, IAM scope, conservative removal sequence, and required tests.

These documents describe decisions and investigation results. They do not establish that the operator is implemented or production-qualified.

Step 1 implementation and measured local results: [qualification findings](qualification/README.md), with [repeatable harness commands](../hack/qualification/README.md).

Step 2: [experimental fleet API and usage](fleet-api.md), including local integration
commands and explicit lifecycle restrictions.

Step 3: [manual lifecycle design](decisions/0012-restart-safe-manual-lifecycle.md)
and [validation results](qualification/lifecycle/README.md).

Step 4: [capacity collection and policy](capacity-policy.md).

Bucket contraction follow-up: [all-candidate preflight and remaining completion
boundary](bucket-scale-in.md). Both manual and automatic removal remain blocked.
