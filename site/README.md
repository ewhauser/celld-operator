# Documentation site

Astro + Starlight site published to GitHub Pages by `.github/workflows/site.yaml`.

```sh
pnpm install
pnpm dev      # syncs, then serves http://localhost:4321/celld-operator/
pnpm build    # syncs, builds dist/, fails on broken internal links
```

`pnpm sync` (run automatically before `dev` and `build`) regenerates everything under
`src/content/docs/{contracts,decisions,qualification,history,api}` from the repository:

| Site section | Source |
| --- | --- |
| Contracts, Historical investigations | `docs/*.md` |
| Design decisions | `docs/decisions/` |
| Qualification evidence | `docs/qualification/**/README.md` and `eks-smoke-plan.md` |
| API references | `config/crd/*.yaml` |
| Sample manifests | `config/samples/*.yaml` |
| Helm chart values | `charts/celld-operator/values.yaml` and `values.schema.json` |
| Operator flags | `cmd/celld-operator/main.go` |

Relative Markdown links between synced documents are rewritten to site routes; links to
files that are not published (logs, JSON evidence, `hack/`) point at GitHub. Those
directories are git-ignored; edit the sources, not the copies.

Hand-written pages live in `src/content/docs/start`, `concepts` and `reference`, and the
landing page is `src/content/docs/index.mdx`. Styling in `src/styles/custom.css` follows
the celld-tck palette. The sidebar for synced sections is generated into
`src/generated-sidebar.json`.

The site builds for `https://ewhauser.github.io/celld-operator/`. Override for a custom
domain with `SITE_URL` and `SITE_BASE`.
