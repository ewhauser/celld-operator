// Sync the repository's Markdown documentation into the Starlight content
// collection and generate the API, chart, sample and flag references.
//
// Everything this script writes is ignored by git (see site/.gitignore); the
// sources of truth stay in ../docs, ../config, ../charts and ../cmd.
import { mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync, existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parse as parseYaml, parseDocument, isMap, isSeq, isScalar } from 'yaml';

const siteDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repoDir = path.resolve(siteDir, '..');
const contentDir = path.join(siteDir, 'src/content/docs');
const repoUrl = 'https://github.com/ewhauser/celld-operator';
const blob = (rel) => `${repoUrl}/blob/main/${rel.split('/').map(encodeURIComponent).join('/')}`;
const tree = (rel) => `${repoUrl}/tree/main/${rel.split('/').map(encodeURIComponent).join('/')}`;

const generatedSections = ['contracts', 'decisions', 'qualification', 'history', 'api'];

// ---------------------------------------------------------------------------
// Manifest: which repository documents become which site pages.
// ---------------------------------------------------------------------------
const contracts = [
	['fleet-api', 'Fleet API'],
	['operations', 'Installation and operations'],
	['capacity-policy', 'Capacity policy'],
	['ordered-bucket', 'Ordered Bucket fleets'],
	['runtime-versions', 'Runtime requirements'],
	['runtime-dependencies', 'Runtime responsibilities'],
	['runtime-control-plane', 'Typed runtime control plane'],
	['launcher-supervision', 'Strict launcher supervision'],
	['current-operation', 'Bounded current operation'],
	['disposable-disks', 'Disposable disks'],
];

/** @type {{source: string, section: string, slug: string, label?: string}[]} */
const pages = [];
for (const [name, label] of contracts) pages.push({ source: `docs/${name}.md`, section: 'contracts', slug: name, label });
// This current user reference has one source; it is not an engineering report.
pages.push({ source: 'docs/critical-features.md', section: 'reference', slug: 'limitations', label: 'Capabilities and limitations' });

pages.push({ source: 'docs/decisions/README.md', section: 'decisions', slug: 'index', label: 'Decision index' });
for (const file of readdirSync(path.join(repoDir, 'docs/decisions')).sort()) {
	if (!/^\d{4}-.*\.md$/.test(file)) continue;
	pages.push({ source: `docs/decisions/${file}`, section: 'decisions', slug: file.replace(/\.md$/, '') });
}

pages.push({ source: 'docs/qualification/README.md', section: 'qualification', slug: 'index', label: 'Qualification index' });
pages.push({ source: 'docs/qualification/eks-smoke-plan.md', section: 'qualification', slug: 'eks-smoke-plan', label: 'EKS smoke suite plan' });
for (const dir of readdirSync(path.join(repoDir, 'docs/qualification')).sort()) {
	const readme = path.join(repoDir, 'docs/qualification', dir, 'README.md');
	if (statSync(path.join(repoDir, 'docs/qualification', dir)).isDirectory() && existsSync(readme)) {
		pages.push({ source: `docs/qualification/${dir}/README.md`, section: 'qualification', slug: dir });
	}
}

const routeOf = (page) => (page.slug === 'index' ? `${page.section}/` : `${page.section}/${page.slug}/`);
const routeBySource = new Map(pages.map((page) => [page.source, routeOf(page)]));
// Documents that exist in the repository but have a hand-written counterpart here.
routeBySource.set('docs/README.md', 'start/overview/');
routeBySource.set('README.md', 'start/overview/');
routeBySource.set('config/samples', 'api/samples/');
for (const file of ['bucket', 'bucket-ordered', 'capacity-shadow', 'maintenance-paused', 'persistent']) {
	routeBySource.set(`config/samples/${file}.yaml`, `api/samples/#${file}`);
}
routeBySource.set('charts/celld-operator/values.yaml', 'api/helm-values/');
routeBySource.set('config/crd/celld.eric.dev_celldfleets.yaml', 'api/celldfleet/');
routeBySource.set('config/crd/celld.eric.dev_celldstoragereservations.yaml', 'api/celldstoragereservation/');

// Link repository Markdown to handwritten guides without publishing duplicate copies.
for (const section of ['start', 'configure', 'operate', 'troubleshoot', 'concepts', 'reference', 'contribute']) {
    const directory = path.join(contentDir, section);
    if (!existsSync(directory)) continue;
    for (const file of readdirSync(directory)) {
        if (!/\.mdx?$/.test(file)) continue;
        const slug = file.replace(/\.mdx?$/, '');
        routeBySource.set(`site/src/content/docs/${section}/${file}`, `${section}/${slug === 'index' ? '' : `${slug}/`}`);
    }
}

// ---------------------------------------------------------------------------
// Markdown transformation
// ---------------------------------------------------------------------------
const relativeRoute = (fromRoute, toRoute) => {
	const [toPath, fragment] = toRoute.split('#');
	const from = path.posix.dirname(`/${fromRoute}index`);
	const to = path.posix.dirname(`/${toPath}index`);
	let rel = path.posix.relative(from, to);
	rel = rel === '' ? './' : `${rel}/`;
	return fragment ? `${rel}#${fragment}` : rel;
};

const rewriteLink = (target, sourceRel, route, warnings) => {
	if (/^(https?:|mailto:|#)/.test(target)) return target;
	const [rawPath, fragment] = target.split('#');
	const resolved = path.posix.normalize(path.posix.join(path.posix.dirname(sourceRel), rawPath));
	const known = routeBySource.get(resolved);
	if (known) {
		const withFragment = fragment && !known.includes('#') ? `${known}#${fragment}` : known;
		return relativeRoute(route, withFragment);
	}
	const abs = path.join(repoDir, resolved);
	if (existsSync(abs)) {
		const url = statSync(abs).isDirectory() ? tree(resolved) : blob(resolved);
		return fragment ? `${url}#${fragment}` : url;
	}
	if (!target.startsWith('/')) warnings.push(`${sourceRel}: unresolved link ${target}`);
	return target;
};

const plainText = (markdown) =>
	markdown
		.replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
		.replace(/[`*_]/g, '')
		.replace(/\s+/g, ' ')
		.trim();

const yamlString = (value) => JSON.stringify(value);

const transform = (page, warnings) => {
	const sourceRel = page.source;
	const route = routeOf(page);
	const text = readFileSync(path.join(repoDir, sourceRel), 'utf8');
	const lines = text.split('\n');
	let title = page.label ?? page.slug;
	let inFence = false;
	let titleStripped = false;
	const out = [];
	let firstParagraph = '';
	let collecting = false;
	for (const line of lines) {
		if (/^\s*(```|~~~)/.test(line)) {
			inFence = !inFence;
			out.push(line);
			continue;
		}
		if (inFence) {
			out.push(line);
			continue;
		}
		if (!titleStripped && /^# /.test(line)) {
			title = line.replace(/^# /, '').trim();
			titleStripped = true;
			continue;
		}
		const rewritten = line
			// A link to an absolute local path (an author's machine) cannot be published; keep the text and show the path.
			.replace(/\[([^\]]+)\]\((\/[^)\s]+)\)/g, (_, text, target) => `${text} (\`${target}\`)`)
			.replace(/\]\(([^)\s]+)\)/g, (_, target) => `](${rewriteLink(target, sourceRel, route, warnings)})`);
		if (titleStripped && !collecting && !firstParagraph && rewritten.trim() && !/^[#|\-*>]/.test(rewritten.trim())) {
			collecting = true;
		}
		if (collecting) {
			if (!rewritten.trim()) collecting = false;
			else firstParagraph = `${firstParagraph} ${plainText(rewritten)}`.trim();
		}
		out.push(rewritten);
	}
	let description = firstParagraph;
	const sentence = description.match(/^.*?[.!?](\s|$)/);
	if (sentence && sentence[0].length >= 40) description = sentence[0].trim();
	if (description.length > 200) description = `${description.slice(0, 197).replace(/\s+\S*$/, '')}…`;
	const label = page.label ?? title.replace(/^ADR \d{4} /, '');
	const internal = ['contracts', 'qualification', 'decisions'].includes(page.section);
	const category = { contracts: 'Implementation detail', qualification: 'Test report', decisions: 'Design record' }[page.section];
	const frontmatter = [
		'---',
		`title: ${yamlString(category ? `${category}: ${title}` : title)}`,
		description ? `description: ${yamlString(description)}` : null,
		`editUrl: ${yamlString(`${repoUrl}/edit/main/${sourceRel}`)}`,
		internal ? 'pagefind: false' : null,
		internal ? 'prev: false\nnext: false' : null,
		'---',
		'',
	]
		.filter((entry) => entry !== null)
		.join('\n');
	const notice = internal
		? `:::note[${category}]\nThis page is for contributors${page.section === 'contracts' ? ' and describes implementation details' : ' and records a particular design or test snapshot'}. For current user guidance, see [capabilities and limitations](${relativeRoute(route, 'reference/limitations/')}) and the [operation guides](${relativeRoute(route, 'operate/scaling/')}). Historical results do not establish current release or AWS validation.\n:::\n\n`
		: '';
	return { label, markdown: frontmatter + notice + out.join('\n').replace(/^\n+/, '') };
};

// ---------------------------------------------------------------------------
// CRD reference
// ---------------------------------------------------------------------------
const cell = (value) => String(value ?? '').replace(/\|/g, '\\|').replace(/\s*\n\s*/g, ' ').trim();
const code = (value) => `\`${String(value).replace(/`/g, '')}\``;

const typeOf = (schema) => {
	if (schema['x-kubernetes-int-or-string']) return 'int or string';
	if (schema.type === 'array') return `[]${typeOf(schema.items ?? {})}`;
	if (schema.type === 'object' && schema.additionalProperties) return `map[string]${typeOf(schema.additionalProperties)}`;
	if (schema.type === 'integer' && schema.format) return schema.format;
	return schema.type ?? 'object';
};

const constraints = (schema) => {
	const notes = [];
	if (schema.enum) notes.push(`One of ${schema.enum.map(code).join(', ')}.`);
	if (schema.default !== undefined) notes.push(`Default ${code(JSON.stringify(schema.default))}.`);
	if (schema.minimum !== undefined || schema.maximum !== undefined) {
		const lo = schema.minimum !== undefined ? schema.minimum : '';
		const hi = schema.maximum !== undefined ? schema.maximum : '';
		notes.push(`Range ${lo}–${hi}.`);
	}
	if (schema.minLength !== undefined || schema.maxLength !== undefined) {
		notes.push(`Length ${schema.minLength ?? 0}–${schema.maxLength ?? '∞'}.`);
	}
	if (schema.minItems !== undefined || schema.maxItems !== undefined) {
		notes.push(`Items ${schema.minItems ?? 0}–${schema.maxItems ?? '∞'}.`);
	}
	if (schema.pattern) notes.push(`Pattern ${code(schema.pattern)}.`);
	if (schema.format && schema.type === 'string') notes.push(`Format ${code(schema.format)}.`);
	return notes.join(' ');
};

// Mirror the heading slugger (github-slugger): lowercase, strip punctuation, spaces to hyphens.
const anchor = (heading) => heading.toLowerCase().replace(/[^a-z0-9 _-]/g, '').replace(/ /g, '-');

const emitObject = (name, schema, sections, depth) => {
	const props = schema.properties ?? {};
	const required = new Set(schema.required ?? []);
	const rows = Object.entries(props).map(([key, child]) => {
		const nested = child.type === 'object' && child.properties ? `${name}.${key}` : child.type === 'array' && child.items?.properties ? `${name}.${key}[]` : null;
		const type = nested ? `[${typeOf(child)}](#${anchor(nested)})` : typeOf(child);
		const desc = [cell(child.description), constraints(child)].filter(Boolean).join(' ');
		return `| ${code(key)} | ${type} | ${required.has(key) ? 'yes' : ''} | ${desc} |`;
	});
	const heading = '#'.repeat(Math.min(depth, 4));
	sections.push(`${heading} ${name}`, '');
	if (schema.description) sections.push(cell(schema.description), '');
	if (rows.length) {
		sections.push('| Field | Type | Required | Description |', '| --- | --- | --- | --- |', ...rows, '');
	}
	const rules = schema['x-kubernetes-validations'];
	if (rules?.length) {
		sections.push('<details>', '<summary>Validation rules and expressions</summary>', '');
		for (const rule of rules) {
			sections.push(`- ${cell(rule.message ?? rule.messageExpression ?? '')}`, '', '  ```text', `  ${rule.rule.replace(/\n/g, ' ')}`, '  ```', '');
		}
		sections.push('</details>', '');
	}
	for (const [key, child] of Object.entries(props)) {
		if (child.type === 'object' && child.properties) emitObject(`${name}.${key}`, child, sections, depth + 1);
		else if (child.type === 'array' && child.items?.properties) emitObject(`${name}.${key}[]`, child.items, sections, depth + 1);
	}
};

const crdPage = (file, slug, intro) => {
	const crd = parseYaml(readFileSync(path.join(repoDir, file), 'utf8'));
	const version = crd.spec.versions[0];
	const schema = version.schema.openAPIV3Schema;
	const kind = crd.spec.names.kind;
	const sections = [
		'---',
		`title: ${yamlString(`${kind} API reference`)}`,
		`description: ${yamlString(`Fields, defaults and validation rules of ${crd.spec.group}/${version.name} ${kind}, generated from the CRD.`)}`,
		`editUrl: ${yamlString(blob(file))}`,
		'---',
		'',
		...intro,
		'',
		`Field names, defaults, and validation rules come from the [CRD](${blob(file)}). Required fields are marked below; optional fields can be omitted unless a validation rule requires them for your profile.`,
		'',
		'| | |',
		'| --- | --- |',
		`| API group | ${code(crd.spec.group)} |`,
		`| Version | ${code(version.name)} |`,
		`| Kind | ${code(kind)} |`,
		`| Scope | ${crd.spec.scope} |`,
		`| Subresources | ${Object.keys(version.subresources ?? {}).map(code).join(', ') || 'none'} |`,
		'',
	];
	if (schema.description) sections.push(cell(schema.description), '');
	if (version.additionalPrinterColumns?.length) {
		sections.push('## Printer columns', '', '| Column | JSON path | Type |', '| --- | --- | --- |');
		for (const column of version.additionalPrinterColumns) sections.push(`| ${column.name} | ${code(column.jsonPath)} | ${column.type} |`);
		sections.push('');
	}
	if (schema['x-kubernetes-validations']?.length) emitObject('Object-level validation', { 'x-kubernetes-validations': schema['x-kubernetes-validations'] }, sections, 2);
	if (schema.properties.spec) emitObject('spec', schema.properties.spec, sections, 2);
	if (schema.properties.status) emitObject('status', schema.properties.status, sections, 2);
	return { slug, markdown: sections.join('\n') };
};

// ---------------------------------------------------------------------------
// Helm values reference
// ---------------------------------------------------------------------------
const helmValuesPage = () => {
	const valuesFile = 'charts/celld-operator/values.yaml';
	const doc = parseDocument(readFileSync(path.join(repoDir, valuesFile), 'utf8'));
	const schema = JSON.parse(readFileSync(path.join(repoDir, 'charts/celld-operator/values.schema.json'), 'utf8'));
	const chart = parseYaml(readFileSync(path.join(repoDir, 'charts/celld-operator/Chart.yaml'), 'utf8'));
	const rows = [];
	const schemaAt = (segments) => {
		let node = schema;
		for (const segment of segments) {
			node = node?.properties?.[segment];
			if (!node) return null;
		}
		return node;
	};
	const requiredAt = (segments) => {
		const parent = segments.length ? schemaAt(segments.slice(0, -1)) : schema;
		return Boolean(parent?.required?.includes(segments.at(-1)));
	};
	const walk = (node, segments, inheritedComment) => {
		if (isMap(node) && node.items.length) {
			for (const pair of node.items) {
				const key = String(pair.key.value ?? pair.key);
				const comment = [pair.key.commentBefore, isScalar(pair.value) ? pair.value.comment : null]
					.filter(Boolean)
					.map((entry) => entry.replace(/^\s*#?\s?/gm, '').replace(/\s*\n\s*/g, ' ').trim())
					.join(' ');
				walk(pair.value, [...segments, key], comment);
			}
			return;
		}
		const value = isSeq(node) || isMap(node) ? node.toJSON() : node?.value;
		const rendered = value === '' ? '`""`' : value === null || value === undefined ? '`null`' : code(JSON.stringify(value));
		const node2 = schemaAt(segments);
		const notes = node2 ? constraints({ ...node2, default: undefined }) : '';
		rows.push(`| ${code(segments.join('.'))} | ${rendered} | ${requiredAt(segments) ? 'yes' : ''} | ${cell([inheritedComment, notes].filter(Boolean).join(' '))} |`);
	};
	walk(doc.contents, [], '');
	const markdown = [
		'---',
		'title: "Helm chart values"',
		`description: ${yamlString('Every value of the celld-operator Helm chart with its default, schema constraint and comment, generated from values.yaml.')}`,
		`editUrl: ${yamlString(blob(valuesFile))}`,
		'---',
		'',
		`Generated from [values.yaml](${blob(valuesFile)}) and [values.schema.json](${blob('charts/celld-operator/values.schema.json')}) at build time. The chart is validated against the schema on install, so a value outside these constraints fails \`helm install\`.`,
		'',
		'| | |',
		'| --- | --- |',
		`| Chart | ${code(chart.name)} ${code(chart.version)} |`,
		`| App version | ${code(chart.appVersion)} |`,
		`| Kubernetes | ${code(chart.kubeVersion)} |`,
		`| Description | ${cell(chart.description)} |`,
		'',
		'Install CRDs explicitly before the first install and on every upgrade; Helm never upgrades or deletes them. Read the [operations contract](../../contracts/operations/) before changing `networkPolicyEnforced`, `launcherImage`.',
		'',
		'## Values',
		'',
		'| Key | Default | Required | Description |',
		'| --- | --- | --- | --- |',
		...rows,
		'',
		'## Rendered resources',
		'',
		'| Template | Purpose |',
		'| --- | --- |',
		...readdirSync(path.join(repoDir, 'charts/celld-operator/templates'))
			.filter((file) => file.endsWith('.yaml'))
			.sort()
			.map((file) => `| [${code(file)}](${blob(`charts/celld-operator/templates/${file}`)}) | ${templatePurpose[file] ?? ''} |`),
		'',
	].join('\n');
	return { slug: 'helm-values', markdown };
};

const templatePurpose = {
	'deployment.yaml': 'Two-replica controller Deployment with leader election, health probes and the opt-in flags rendered from values.',
	'metrics.yaml': 'Optional metrics Service, ServiceMonitor and PrometheusRule (`metrics.*`).',
	'pdb.yaml': 'PodDisruptionBudget keeping one controller replica available.',
	'rbac.yaml': 'ClusterRole limited to the fleet API and cluster-scoped storage and node objects, plus the leader-election Role.',
	'rbac-fleet-namespaces.yaml': 'The namespaced fleet Role and RoleBinding rendered into every entry of `fleetNamespaces`.',
	'serviceaccount.yaml': 'Controller ServiceAccount for Kubernetes access; it needs no AWS IAM role.',
};

// ---------------------------------------------------------------------------
// Samples and operator flags
// ---------------------------------------------------------------------------
const sampleNotes = {
	'bucket.yaml': 'Three-replica Bucket fleet spread strictly across three zones. The minimal starting point.',
	'bucket-ordered.yaml': 'Bucket fleet using the Ordered StatefulSet layout with deterministic highest-ordinal retirement, across two zones.',
	'capacity-shadow.yaml': 'A Bucket fleet with a `capacity` block in `Shadow` mode: recommendations are recorded and reported, nothing scales.',
	'maintenance-paused.yaml': 'PersistentFleet with maintenance paused and a commented restart token, showing the request fields.',
	'persistent-fleet.yaml': '',
	'persistent.yaml': 'Three-replica PersistentFleet on a Delete-policy CSI StorageClass with strict placement.',
};

const samplesPage = () => {
	const files = readdirSync(path.join(repoDir, 'config/samples')).filter((file) => file.endsWith('.yaml')).sort();
	const out = [
		'---',
		'title: "Sample manifests"',
		'description: "The CelldFleet examples shipped in config/samples, with what each one demonstrates and what must be replaced before applying it."',
		`editUrl: ${yamlString(tree('config/samples'))}`,
		'---',
		'',
		'Samples are not deployable as written. Set an explicit verified compatible fork runtime digest, replace the bucket name with a dedicated bucket, create the referenced ServiceAccount with a runtime IAM identity, and use zone names in the storage region. Every sample carries `qualification: Experimental`, which the API requires.',
		'',
	];
	for (const file of files) {
		const id = file.replace(/\.yaml$/, '');
		out.push(`## ${id}`, '');
		if (sampleNotes[file]) out.push(sampleNotes[file], '');
		out.push(`\`\`\`yaml title="config/samples/${file}"`, readFileSync(path.join(repoDir, 'config/samples', file), 'utf8').trimEnd(), '```', '');
	}
	return { slug: 'samples', markdown: out.join('\n') };
};

const flagsPage = () => {
	const source = 'cmd/celld-operator/main.go';
	const text = readFileSync(path.join(repoDir, source), 'utf8');
	const rows = [];
	for (const match of text.matchAll(/fs\.(Bool|String|Int)\("([^"]+)",\s*([^,]+),\s*"((?:[^"\\]|\\.)*)"\)/g)) {
		const [, kind, name, def, usage] = match;
		rows.push(`| ${code(`--${name}`)} | ${kind.toLowerCase()} | ${code(def.trim())} | ${cell(usage)} |`);
	}
	const markdown = [
		'---',
		'title: "Operator flags"',
		'description: "Command-line flags of the celld-operator binary, generated from cmd/celld-operator/main.go."',
		`editUrl: ${yamlString(blob(source))}`,
		'---',
		'',
		`Generated from [main.go](${blob(source)}) at build time. The binary accepts no positional arguments and exits on unknown flags. Flags prefixed \`--local-\` belong to the disposable integration harness and are rejected outside \`--local-test\`.`,
		'',
		'| Flag | Type | Default | Description |',
		'| --- | --- | --- | --- |',
		...rows,
		'',
		'## Combinations the binary rejects',
		'',
		'- `--local-rwop` or `--local-fault-point` without `--local-test`.',
		'',
		'## Listeners',
		'',
		'| Port | Purpose |',
		'| --- | --- |',
		'| `:8082` | Health probes `/healthz` and `/readyz`. |',
		'| `--metrics-bind-address` | Prometheus metrics, disabled by default. The Helm chart uses port 8084. |',
		'',
		'Leader election uses a Lease named `celld-operator.celld.eric.dev` in `--operator-namespace`.',
		'',
	].join('\n');
	return { slug: 'operator-flags', markdown };
};

// ---------------------------------------------------------------------------
// Write everything
// ---------------------------------------------------------------------------
for (const section of generatedSections) rmSync(path.join(contentDir, section), { recursive: true, force: true });

const warnings = [];
const sidebar = { contracts: [], qualification: [], decisions: [] };
let count = 0;
for (const page of pages) {
	const { label, markdown } = transform(page, warnings);
	const target = path.join(contentDir, page.section, `${page.slug}.md`);
	mkdirSync(path.dirname(target), { recursive: true });
	writeFileSync(target, markdown);
	if (sidebar[page.section]) sidebar[page.section].push({ label, slug: page.slug === 'index' ? page.section : `${page.section}/${page.slug}` });
	count += 1;
}

const generated = [
	crdPage('config/crd/celld.eric.dev_celldfleets.yaml', 'celldfleet', [
		'A `CelldFleet` describes one celld fleet in a namespace: its profile, replica target, storage bucket, placement, optional capacity policy and maintenance requests. Only `replicas`, `capacity`, `runtimeImage` and `maintenance` are mutable after creation. See the [fleet API contract](../../contracts/fleet-api/) for semantics and the [conditions reference](../../reference/conditions/) for what status reports.',
	]),
	crdPage('config/crd/celld.eric.dev_celldstoragereservations.yaml', 'celldstoragereservation', [
		'A `CelldStorageReservation` is the cluster-scoped, never garbage-collected tombstone that binds a bucket to exactly one fleet identity and carries bounded current-operation authority. The operator creates it; administrators read it. Never delete one to reuse a bucket or a retained disk. See [current operations](../../concepts/current-operation/) and [the exact disk contract](../../contracts/disposable-disks/).',
	]),
	samplesPage(),
	helmValuesPage(),
	flagsPage(),
];
for (const page of generated) {
	mkdirSync(path.join(contentDir, 'api'), { recursive: true });
	writeFileSync(path.join(contentDir, 'api', `${page.slug}.md`), page.markdown);
	count += 1;
}

writeFileSync(path.join(siteDir, 'src/generated-sidebar.json'), `${JSON.stringify(sidebar, null, '\t')}\n`);

for (const warning of warnings) console.warn(`warning: ${warning}`);
console.log(`synced ${count} pages into ${path.relative(siteDir, contentDir)}`);
