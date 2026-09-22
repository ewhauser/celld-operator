# Release security

This repository applies the relevant controls from
[Astral's open-source security guidance](https://astral.sh/blog/open-source-security-at-astral).
The implementation also draws on [gbash](https://github.com/ewhauser/gbash).

## Publishing

Run the **Release** workflow against `main`, supplying a new version such as
`v0.2.0-rc.1`. Do not create or push a tag yourself:

```sh
gh workflow run release.yaml --ref main -f version=v0.2.0-rc.1
```

Every job checks out the dispatch's fixed commit SHA. Validation must pass before
the image publication job requests approval in the `release` environment. After
approval, that job builds without restored caches, publishes the image, smoke
tests both architectures, and records signed provenance. The lifecycle suite
then qualifies that exact image digest in Kind.

The final job requests approval in `release-artifacts`. It packages the chart
with the qualified image digest, verifies the registry round trip, signs both OCI
artifacts, and attests the release files. Only then does it create the Git tag,
upload all assets to a draft, and publish the immutable GitHub release. The tag
ruleset requires the earlier `release` deployment to have succeeded.
Stable version tags publish regular releases; version tags with a prerelease
suffix publish prereleases.

Both environments allow only the `main` branch and prohibit administrator
bypass of approval. Eric approves both stages. Self-approval is intentionally
allowed because this repository currently has one maintainer. Add another
reviewer and enable `prevent_self_review` when a second maintainer is available.

Publication refuses an existing Git tag, GitHub release, image tag, or chart
version. If a run fails after publishing any artifact, inspect the failure and
use a new version. Do not overwrite or delete the partial publication. A failed
run may leave an image, chart, protected tag, or draft; rerunning the whole
workflow with the same version will fail its publication guard.

No registry PAT or signing key is needed. GHCR uses the job's short-lived
`GITHUB_TOKEN`; Sigstore uses GitHub OIDC. Any future credentials belong in a
protected deployment environment, with only the permissions the job needs.

## Verifying downloads

For releases produced by this workflow, download the assets and verify the
checksum file's signing identity before trusting its contents:

```sh
identity='https://github.com/ewhauser/celld-operator/.github/workflows/release.yaml@refs/heads/main'
issuer='https://token.actions.githubusercontent.com'
cosign verify-blob --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" \
  --bundle SHA256SUMS.sigstore.json SHA256SUMS
sha256sum --check SHA256SUMS
cosign verify --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" "$(cat image.txt)"
cosign verify --certificate-identity "$identity" \
  --certificate-oidc-issuer "$issuer" "$(cat chart.txt)"
gh attestation verify "oci://$(cat image.txt)" \
  --repo ewhauser/celld-operator \
  --signer-workflow ewhauser/celld-operator/.github/workflows/release.yaml \
  --source-ref refs/heads/main
gh attestation verify SHA256SUMS --repo ewhauser/celld-operator \
  --signer-workflow ewhauser/celld-operator/.github/workflows/release.yaml \
  --source-ref refs/heads/main
```

Use `shasum -a 256 --check SHA256SUMS` on macOS. Match the workflow identity
exactly; accepting any workflow in the repository weakens the guarantee.
Earlier releases used a tag-based identity and do not gain these controls
retroactively. GitHub immutability covers GitHub release assets and associated
tags; GHCR tags are still mutable at the registry, so consume OCI digests and
verify signatures. The Kind suite exercises the published artifact against local storage; see the
[test records](qualification/README.md) for its scope.

## CI and dependencies

All workflows start with no permissions. Jobs receive explicit permissions, and
checkout never persists credentials. Pull requests run without publishing
credentials. `pull_request_target` and `workflow_run` are forbidden by the
security tests; external-contributor workflows require approval in GitHub.
Automation that needs to comment on untrusted contributions should use a
separate GitHub App instead of a privileged Actions trigger.

`make security-check` runs actionlint, zizmor, and security regression tests.
With `GH_TOKEN` set, zizmor also checks action commit ownership and version
comments against GitHub. The Security workflow supplies its read-only token and
runs on every PR, every main push, and weekly. It also runs Go and npm advisory
scans. Run `make vuln-check` locally for the Go scan.

Actions, including transitive composite actions, must use full commit SHAs.
`hack/ci-tools.json` pins Helm, kind, kubectl, and Buildx download hashes; CI
verifies bytes before installation. Cosign's pinned installer embeds the chosen
version's hashes. BuildKit, QEMU, the Dockerfile frontend, base images, and the
SBOM generator are digest-pinned. Release Go setup, Docker builds, and the
documentation deployment do not restore shared build caches.

Go dependencies use `go.sum`; Python tooling uses version and wheel hash locks;
the site uses its frozen pnpm lockfile. Renovate proposes routine updates after
three days, with no automatic merge. Known vulnerability fixes bypass that
Renovate delay for prompt review. pnpm also delays newly resolved packages by
three days; a time-sensitive security update may need a reviewed, package-specific
exception there. Review new dependencies, install scripts, binary downloads,
and action internals when updating pins. Update tool hashes alongside versions;
never fetch the expected hash dynamically during installation. Report upstream
security problems privately and contribute fixes where practical.

## GitHub settings

`.github/repository-security.json` is the reviewed configuration for this
repository. Preview it, or reapply it with repository administration access:

```sh
python3 hack/configure-repository-security.py
python3 hack/configure-repository-security.py --apply
```

It configures read-only workflow defaults, full-SHA action enforcement, fork
approval, immutable releases, private vulnerability reporting, Dependabot
alerts/security fixes, secret scanning/push protection, and these rulesets:

- `main`: PRs, resolved review discussions, and passing build, test, lint, chart,
  security, and lifecycle checks; no force pushes, deletion, or bypass actors.
- `immutable-tags`: no tag updates or deletion, with no bypass actors.
- `release-tags-require-approval`: `v*` tags require a successful `release`
  deployment.
- `private-security-work`: block public advisory, GHSA, CVE, and internal branch
  names. Develop embargoed fixes in a private security advisory fork.

These are repository controls, not organization-enforced controls: the owner can
still edit them. Enforcing phishing-resistant account authentication, preventing
repository administrators from editing policy, and applying organization-wide
trigger restrictions require account/organization administration outside this
repository. Keep maintainer access minimal and use passkeys or security keys.
Financial support and relationships with upstream maintainers remain maintainer
decisions rather than CI settings.
