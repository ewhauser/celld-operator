# Documentation site

Astro + Starlight user documentation. The primary navigation follows tasks:
Get started, Configure, Operate, Troubleshoot, Reference, and Contribute.

```sh
pnpm install --frozen-lockfile
pnpm dev --background  # sync, then serve http://localhost:4321/celld-operator/
pnpm exec astro dev stop
pnpm build             # generate references, build search, validate internal links
```

Write user guides in `src/content/docs/{start,configure,operate,troubleshoot}`.
The homepage is `src/content/docs/index.md`. Concepts and contributor guidance
live under `concepts` and `contribute`; the sidebar is in `astro.config.mjs`.

`pnpm sync` runs before dev/build. It copies engineering records from `docs/` into
ignored `contracts`, `decisions` and `qualification` directories.
Those pages remain accessible through Contribute, carry audience
labels, and are excluded from user search. Edit their repository sources.

The current capability matrix has one source: `docs/critical-features.md`, published
as `reference/limitations`. Keep implementation status and validation boundaries separate.

API, Helm, sample and flag references under `api` are generated from Go-generated
CRDs, chart values, sample manifests and the binary flags. Add field descriptions
in `api/v1alpha1`, then run `make generate` and `make chart-sync` at repository root.
Raw validation expressions are available in expandable details.

Check a documentation change by reading its commands against the current code,
running `pnpm build`, and opening the result on desktop and a narrow viewport.
Verify the first-application example and key search terms. A site build checks
links; it does not establish that the guide was executed on EKS.

GitHub Pages publication is configured in `.github/workflows/site.yaml` for
`https://ewhauser.github.io/celld-operator/`. Override both `SITE_URL` and `SITE_BASE`
for another deployment. A successful local build does not publish the site.
