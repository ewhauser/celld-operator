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
                { label: 'Get started', items: [
                    { label: 'Overview', slug: 'start/overview' },
                    { label: 'Prerequisites', slug: 'start/prerequisites' },
                    { label: 'Install the operator', slug: 'start/install' },
                    { label: 'Create a fleet', slug: 'start/first-fleet' },
                    { label: 'Run your first application', slug: 'start/first-application' },
                    { label: 'Check fleet health', slug: 'start/verify' },
                ] },
                { label: 'Configure', collapsed: true, items: [
                    { label: 'Choose a storage profile', slug: 'configure/profiles' },
                    { label: 'AWS permissions', slug: 'configure/aws' },
                    { label: 'Storage', slug: 'configure/storage' },
                    { label: 'Networking', slug: 'configure/networking' },
                    { label: 'Availability zones', slug: 'configure/placement' },
                ] },
                { label: 'Operate', collapsed: true, items: [
                    { label: 'Scale a fleet', slug: 'operate/scaling' },
                    { label: 'Enable capacity policy', slug: 'operate/capacity' },
                    { label: 'Restart a fleet', slug: 'operate/restart' },
                    { label: 'Upgrade the operator', slug: 'operate/upgrade-operator' },
                    { label: 'Upgrade the runtime', slug: 'operate/upgrade-runtime' },
                    { label: 'Monitor a fleet', slug: 'operate/monitoring' },
                    { label: 'Delete a fleet and retain data', slug: 'operate/deletion' },
                ] },
                { label: 'Troubleshoot', collapsed: true, items: [
                    { label: 'Find your symptom', slug: 'troubleshoot' },
                    { label: 'Installation and access', slug: 'troubleshoot/installation' },
                    { label: 'Pending or unready pods', slug: 'troubleshoot/scheduling' },
                    { label: 'Blocked operations', slug: 'troubleshoot/lifecycle' },
                ] },
                { label: 'Reference', collapsed: true, items: [
                    { label: 'Capabilities and limitations', slug: 'reference/limitations' },
                    { label: 'Supported versions', slug: 'reference/compatibility' },
                    { label: 'Conditions and reasons', slug: 'reference/conditions' },
                    { label: 'CelldFleet API', slug: 'api/celldfleet' },
                    { label: 'Helm values', slug: 'api/helm-values' },
                    { label: 'Sample manifests', slug: 'api/samples' },
                    { label: 'Operator flags', slug: 'api/operator-flags' },
                    { label: 'Security', slug: 'reference/security-boundaries' },
                    { label: 'Storage reservation API', slug: 'api/celldstoragereservation' },
                ] },
                { label: 'Contribute', collapsed: true, items: [
                    { label: 'Development and documentation', slug: 'contribute' },
                    { label: 'Architecture', slug: 'concepts/architecture' },
                    { label: 'How operations resume', slug: 'concepts/lifecycle-journal' },
                    { label: 'How removal is checked', slug: 'concepts/safety-model' },
                    { label: 'Test reports', collapsed: true, items: sidebar.qualification },
                    { label: 'Design records', collapsed: true, items: sidebar.decisions },
                    { label: 'Historical investigations', collapsed: true, items: sidebar.history },
                ] },
            ],
		}),
	],
});
