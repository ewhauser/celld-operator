// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import starlightLinksValidator from 'starlight-links-validator';
import { readFileSync } from 'node:fs';

const repo = 'https://github.com/ewhauser/celld-operator';
// GitHub Pages serves a project site under /celld-operator. Override both for a
// custom domain: SITE_URL=https://docs.example.com SITE_BASE=/ pnpm build
const site = process.env.SITE_URL ?? 'https://ewhauser.github.io';
const base = process.env.SITE_BASE ?? '/celld-operator';
const sidebar = JSON.parse(readFileSync(new URL('./src/generated-sidebar.json', import.meta.url), 'utf8'));

export default defineConfig({
	site,
	base,
	trailingSlash: 'always',
	integrations: [
		starlight({
			title: 'celld operator',
			description: 'Experimental Kubernetes fleet operator for celld on EKS, S3 and EBS.',
			favicon: '/favicon.svg',
			social: [{ icon: 'github', label: 'GitHub', href: repo }],
			editLink: { baseUrl: `${repo}/edit/main/site/` },
			lastUpdated: true,
			customCss: ['./src/styles/custom.css'],
			components: {
				SiteTitle: './src/components/SiteTitle.astro',
				Footer: './src/components/Footer.astro',
			},
			head: [
				{ tag: 'link', attrs: { rel: 'preconnect', href: 'https://fonts.googleapis.com' } },
				{ tag: 'link', attrs: { rel: 'preconnect', href: 'https://fonts.gstatic.com', crossorigin: '' } },
				{
					tag: 'link',
					attrs: {
						rel: 'stylesheet',
						href: 'https://fonts.googleapis.com/css2?family=Inter:wght@400;450;500;550;600;650&display=swap',
					},
				},
			],
			plugins: [
				starlightLinksValidator({
					errorOnRelativeLinks: false,
					errorOnLocalLinks: false,
				}),
			],
			sidebar: [
				{
					label: 'Start here',
					items: [
						{ label: 'What celld operator is', slug: 'start/overview' },
						{ label: 'Install', slug: 'start/install' },
						{ label: 'Your first fleet', slug: 'start/first-fleet' },
						{ label: 'Verify and observe', slug: 'start/verify' },
						{ label: 'Project status', slug: 'contracts/critical-features' },
					],
				},
				{
					label: 'Concepts',
					items: [
						{ label: 'Architecture', slug: 'concepts/architecture' },
						{ label: 'Fleet profiles', slug: 'concepts/profiles' },
						{ label: 'Lifecycle journal', slug: 'concepts/lifecycle-journal' },
						{ label: 'Safety model', slug: 'concepts/safety-model' },
					],
				},
				{ label: 'Contracts', items: sidebar.contracts },
				{
					label: 'Reference',
					items: [
						{ label: 'CelldFleet API', slug: 'api/celldfleet' },
						{ label: 'CelldStorageReservation API', slug: 'api/celldstoragereservation' },
						{ label: 'Sample manifests', slug: 'api/samples' },
						{ label: 'Helm chart values', slug: 'api/helm-values' },
						{ label: 'Operator flags', slug: 'api/operator-flags' },
						{ label: 'Conditions and blockers', slug: 'reference/conditions' },
						{ label: 'Compatibility', slug: 'reference/compatibility' },
						{ label: 'Security boundaries', slug: 'reference/security-boundaries' },
					],
				},
				{ label: 'Qualification evidence', items: sidebar.qualification },
				{ label: 'Design decisions', collapsed: true, items: sidebar.decisions },
				{ label: 'Historical investigations', collapsed: true, items: sidebar.history },
			],
		}),
	],
});
