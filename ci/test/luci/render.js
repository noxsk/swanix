#!/usr/bin/env node
/* Render every swanix LuCI view in headless Chromium against LuCI's own JS runtime
 * and a stubbed ubus backend. It proves the views load, bind to the real UCI
 * config and render the status data; it does not replace a test on a router.
 *
 * Usage: node render.js <luci-base resources dir> <out dir>
 *   luci-base resources: feeds/luci/modules/luci-base/htdocs/luci-static/resources
 */
'use strict';
const http = require('http');
const fs = require('fs');
const path = require('path');
const { chromium } = require('playwright');
const assert = require('assert/strict');

const [luciResDir, outDir] = process.argv.slice(2);
if (!luciResDir || !outDir) { console.error('usage: render.js <luci-base resources> <out dir>'); process.exit(2); }
const luciRes = path.resolve(luciResDir);
const repo = path.resolve(__dirname, '../../..');
const appRes = path.join(repo, 'openwrt/luci-app-swanix/htdocs/luci-static/resources');
fs.mkdirSync(outDir, { recursive: true });
const translations = Object.fromEntries([...fs.readFileSync(path.join(repo,
	'openwrt/luci-app-swanix/po/zh_Hans/swanix.po'), 'utf8').matchAll(/^msgid (".+")\nmsgstr (".+")$/gm)]
	.map(m => [JSON.parse(m[1]), JSON.parse(m[2])]));

/* ---- UCI fixture: the shipped default config, as ubus `uci get` returns it ---- */
function parseUci(text) {
	const values = {}; let cur = null; let idx = 0;
	for (const raw of text.split('\n')) {
		const line = raw.replace(/#.*$/, '').trim();
		if (!line) continue;
		const w = line.match(/'[^']*'|"[^"]*"|\S+/g).map(s => s.replace(/^['"]|['"]$/g, ''));
		if (w[0] === 'config') {
			cur = { '.anonymous': false, '.type': w[1], '.name': w[2], '.index': idx++ };
			values[w[2]] = cur;
		} else if (w[0] === 'option') cur[w[1]] = w[2];
		else if (w[0] === 'list') (cur[w[1]] = cur[w[1]] || []).push(w[2]);
	}
	return values;
}
const uciValues = parseUci(fs.readFileSync(path.join(repo, 'openwrt/swanix-common/files/etc/config/swanix'), 'utf8'));

/* ---- ubus fixtures ---- */
const now = Date.now();
const status = {
	version: '0.0.0-test', profile: process.env.SWANIX_PROFILE || 'full', enabled: true, mode: 'manage', managing: true,
	generated_at: new Date(now).toISOString(), rejected_connections: 3, problems: ['wan.wan2.network: netifd 接口 wan2 不存在'],
	wans: [
		{ id: 1, name: 'wan1', network: 'wan', device: 'eth1', download_capacity_mbps: 500, upload_capacity_mbps: 50, rx_mbps: 120.4, tx_mbps: 8.2, assigned_connections: 4210,
		  families: [{ family: 4, state: 'healthy', reason: 'probes succeeded', since: new Date(now - 60000).toISOString(), rtt_ms: 18, base_rtt_ms: 16, probe_failure_rate: 0.02, new_connection_share: { balanced: 62 } }] },
		{ id: 2, name: 'wan2', network: 'wan2', device: 'eth2', download_capacity_mbps: 300, upload_capacity_mbps: 30, rx_mbps: 0, tx_mbps: 0, assigned_connections: 130,
		  families: [{ family: 4, state: 'recovering', reason: 'probes recovered', since: new Date(now - 5000).toISOString(), rtt_ms: 60, base_rtt_ms: 35, probe_failure_rate: 0.3, new_connection_share: { balanced: 8 } }] },
	],
	recent_transitions: [{ at: new Date(now - 5000).toISOString(), wan: 'wan2', family: 4, from: 'down', to: 'recovering', reason: 'probes recovered' }],
};
const history = Array.from({ length: 120 }, (_, i) => ({ at: new Date(now - (120 - i) * 5000).toISOString(),
	rx_mbps: { wan1: 80 + 40 * Math.sin(i / 9), wan2: 30 + 20 * Math.cos(i / 7) }, tx_mbps: { wan1: 6 + Math.sin(i / 5), wan2: 2 } }));
const interfaceHistory = { interval_seconds: 30, retention_seconds: 21600,
	points: Array.from({ length: 720 }, (_, i) => ({ at: new Date(now - (719 - i) * 30000).toISOString(), interfaces: [
		{ wan: 'wan1', network: 'wan', device: 'eth1', family: 4, state: 'healthy', reason: 'probes succeeded', managing: true,
			rtt_ms: 18 + 3 * Math.sin(i / 10), probe_failure_rate: 0, down_events: i === 300 ? 1 : 0 },
		{ wan: 'wan1', network: 'wan', device: 'eth1', family: 6, state: 'healthy', reason: 'probes succeeded', managing: true,
			rtt_ms: 45, probe_failure_rate: 0, down_events: 0 },
		{ wan: 'wan2', network: 'wan2', device: 'eth2', family: 4, state: i >= 100 && i < 110 ? 'down' : i >= 110 && i < 115 ? 'recovering' : 'healthy',
			reason: i >= 100 && i < 110 ? 'probes failed' : 'probes succeeded', managing: true,
			rtt_ms: i >= 100 && i < 110 ? null : 60, probe_failure_rate: i >= 100 && i < 110 ? 1 : 0, down_events: i === 100 ? 1 : 0 },
		{ wan: 'wan2', network: 'wan2', device: 'eth2', family: 6, state: 'healthy', reason: 'observe: netifd link state only, not probed',
			managing: false, rtt_ms: null, probe_failure_rate: null, down_events: 0 }
	] })) };
const features = { version: '0.0.0-test', profile: status.profile, upstream_version: 'v1.14.4', upstream_commit: 'c005f35300ce76427dc285481ec5695f5b179cd2',
	commit: 'deadbeef', go_version: 'go1.26.10', os: 'linux', arch: 'amd64', cgo: '1', build_tags: ['with_quic', 'with_musl'],
	multiwan_features: ['ipv4-connection-balancing'], proxy_features: status.profile === 'full' ? ['inbound:socks', 'outbound:direct', 'outbound:vless'] : [], verified_scope: [] };
const doctor = { ok: false, checks: [
	{ name: 'tool:nft', level: 'ok', detail: '可用' },
	{ name: 'fw4-masq:wan1', level: 'fail', detail: 'WAN 所在防火墙区域未启用 masq', fix: '在该 zone 上设置 masq 1' },
	{ name: 'rp_filter:eth1', level: 'warn', detail: '严格反向路径过滤', fix: '应用时会调为 loose' } ] };

const calls = []; const unknown = new Set();
function ubus(obj, method, args) {
	calls.push(`${obj}.${method}`);
	switch (`${obj}.${method}`) {
	case 'session.access': return { access: true, 'access-group': { 'luci-app-swanix': ['read', 'write'] } };
	case 'uci.get': return args.config === 'swanix' ? { values: uciValues } : { values: {} };
	case 'uci.state': case 'uci.changes': return {};
	case 'swanix.status': return status;
	case 'swanix.history': return { points: history };
	case 'swanix.interface_history': return interfaceHistory;
	case 'swanix.features': case 'swanix.version': return features;
	case 'swanix.doctor': return doctor;
	case 'swanix.preview': return { ok: true, text: 'ip -4 rule add fwmark 0x01000000/0x0f000000 priority 10201 lookup 1001\n' };
	case 'swanix.check': return { ok: true, text: 'OK' };
	case 'swanix.prepare': case 'swanix.apply': case 'swanix.confirm': case 'swanix.rollback': return { ok: true, text: 'ok' };
	case 'network.interface.dump': return { interface: [
		{ interface: 'wan', up: true, proto: 'dhcp', device: 'eth1', l3_device: 'eth1', 'ipv4-address': [{ address: '192.0.2.2', mask: 24 }], route: [] },
		{ interface: 'wan2', up: true, proto: 'dhcp', device: 'eth2', l3_device: 'eth2', 'ipv4-address': [{ address: '198.51.100.2', mask: 24 }], route: [] },
		{ interface: 'lan', up: true, proto: 'static', device: 'br-lan', l3_device: 'br-lan', 'ipv4-address': [{ address: '192.168.1.1', mask: 24 }], route: [] } ] };
	case 'luci-rpc.getNetworkDevices': return { eth1: { name: 'eth1', type: 'Network device' }, eth2: { name: 'eth2', type: 'Network device' }, 'br-lan': { name: 'br-lan', type: 'Network device' } };
	case 'luci-rpc.getBoardJSON': return { network: {} };
	case 'luci-rpc.getHostHints': case 'luci-rpc.getDHCPLeases': return {};
	case 'network.device.status': return {};
	case 'network.wireless.status': return {};
	}
	unknown.add(`${obj}.${method}`);
	return {};
}

const server = http.createServer((req, res) => {
	if (req.url === '/favicon.ico') { res.writeHead(204); res.end(); return; }
	if (req.method === 'POST' && req.url.startsWith('/ubus')) {
		let body = ''; req.on('data', c => body += c); req.on('end', () => {
			const reqs = JSON.parse(body); const list = Array.isArray(reqs) ? reqs : [reqs];
			const out = list.map(r => {
				if (r.method === 'list') return { jsonrpc: '2.0', id: r.id, result: {} };
				if (!Array.isArray(r.params)) { unknown.add('malformed:' + JSON.stringify(r).slice(0, 80)); return { jsonrpc: '2.0', id: r.id, result: [0, {}] }; }
				const [, obj, method, args] = r.params; return { jsonrpc: '2.0', id: r.id, result: [0, ubus(obj, method, args || {})] };
			});
			res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify(Array.isArray(reqs) ? out : out[0]));
		}); return;
	}
	const u = decodeURIComponent(req.url.split('?')[0]);
	if (u === '/') {
		const view = new URL(req.url, 'http://x').searchParams.get('view');
		res.setHeader('Content-Type', 'text/html');
		res.end(`<!doctype html><html><head><meta charset="utf-8"></head><body>
<script src="/luci-static/resources/cbi.js"></script>
${new URL(req.url, 'http://x').searchParams.get('lang') === 'zh_Hans' ? `<script>window.TR = {}; Object.entries(${JSON.stringify(translations)}).forEach(([k,v]) => TR[sfh(trimws(k))] = v);</script>` : ''}
<script src="/luci-static/resources/luci.js"></script>
<script>L = new LuCI({ media:'/luci-static/bootstrap', resource:'/luci-static/resources', scriptname:'/cgi-bin/luci', pathinfo:'/admin/network/swanix',
 requestpath:['admin','network','swanix'], dispatchpath:['admin','network','swanix'], pollinterval:5, ubuspath:'/ubus/', sessionid:'00000000000000000000000000000000',
 token:'t', nodespec:{}, apply_rollback:90, apply_holdoff:4, apply_timeout:5, apply_display:1.5, rollback_token:'' });</script>
<div id="maincontent"><div id="view"></div></div>
<script>L.require('ui').then(function(ui){ ui.instantiateView('${view}'); });</script></body></html>`);
		return;
	}
	if (u.startsWith('/luci-static/resources/')) {
		const rel = u.slice('/luci-static/resources/'.length);
		for (const base of [appRes, luciRes]) {
			const f = path.join(base, rel);
			if (f.startsWith(base) && fs.existsSync(f) && fs.statSync(f).isFile()) { res.setHeader('Content-Type', 'application/javascript'); res.end(fs.readFileSync(f)); return; }
		}
	}
	res.statusCode = 404; res.end('not found: ' + u);
});

(async () => {
	await new Promise(r => server.listen(0, '127.0.0.1', r));
	const base = `http://127.0.0.1:${server.address().port}`;
	const browser = await chromium.launch({ channel: process.env.PLAYWRIGHT_CHROMIUM_CHANNEL || undefined });
	const pages = [
		['swanix.overview', /Multi-WAN overview/, ['wan1', 'recovering', '62%', 'probes recovered']],
		['swanix.interface_history', /Interface history/, ['wan1 / IPv4', 'State history', '21.0 ms', '100.0%']],
		['swanix.wans', /Lines \(WAN\)/, ['wan1', 'wan2', 'Capacities']],
		['swanix.policies', /Policies and rules/, ['balanced']],
		['swanix.health', /Health checks/, ['health_default', '1.1.1.1:443']],
		['swanix.ipv6', /IPv6/, ['native']],
		['swanix.proxy', /Proxy \(Full build\)/, ['Compiled-in proxy features (3)', 'Close connections on a failed line']],
		['swanix.system', /System \/ Diagnostics/, ['fw4-masq:wan1', 'go1.26.10']],
	];
	let failed = 0;
	for (const [view, titleRe, needles] of pages) {
		if (view === 'swanix.proxy' && status.profile !== 'full') continue;
		const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
		const errors = [];
		page.on('pageerror', e => errors.push('pageerror: ' + e.message));
		page.on('console', m => { if (m.type() === 'error') errors.push('console: ' + m.text()); });
		page.on('requestfailed', r => errors.push('requestfailed: ' + r.url()));
		page.on('response', r => { if (r.status() >= 400) errors.push(`HTTP ${r.status()}: ${r.url()}`); });
		await page.goto(`${base}/?view=${encodeURIComponent(view)}`);
		try { await page.waitForFunction(() => !document.querySelector('#view .spinning') && document.querySelector('#view').children.length > 0, null, { timeout: 15000 }); }
		catch (e) { throw new Error(`${view} never finished loading: ${errors.join('; ')}`, { cause: e }); }
		await page.waitForTimeout(600);
		const text = await page.locator('#view').innerText().catch(() => '');
		await page.screenshot({ path: path.join(outDir, view + '.png'), fullPage: true });
		const missing = needles.filter(n => !text.includes(n));

		if (view === 'swanix.ipv6') {
			await page.selectOption('select[id="widget.cbid.swanix.ipv6.mode"]', 'nat66');
			const source = page.locator('[data-field="cbid.swanix.ipv6.nat66_source"]');
			await source.waitFor({ state: 'visible' });
			assert.ok((await source.locator('input').evaluateAll(inputs => inputs.map(i => i.value))).includes('fd00::/64'));
			const policy = page.locator('select[id="widget.cbid.swanix.ipv6.nat66_policy"]');
			await policy.selectOption('balanced');
			assert.equal(await policy.inputValue(), 'balanced');
			console.log('ok   NAT66 prefix default and egress policy selector');
		}
		if (view === 'swanix.wans') {
			await page.locator('#view [data-section-id="wan1"] button:has-text("Edit")').click();
			await page.waitForSelector('body.modal-overlay-active');
			const family = page.locator('.modal select[id="widget.cbid.swanix.wan1.ip_family"]');
			assert.equal(await family.inputValue(), 'dual');
			await family.selectOption('ipv4');
			await page.waitForFunction(() => document.querySelector('.modal [data-field="cbid.swanix.wan1.ipv6_network"]').classList.contains('hidden'));
			await family.selectOption('ipv6');
			await page.locator('.modal select[id="widget.cbid.swanix.wan1.ipv6_network"]').waitFor({ state: 'visible' });
			await page.locator('.modal button:has-text("Dismiss")').click();
			console.log('ok   line address family and IPv6 interface dependencies');
		}
		if (view === 'swanix.overview') {
			const charts = await page.locator('#view svg').evaluateAll(charts => charts.map(svg =>
				Array.from(svg.querySelectorAll('polyline'), line => line.points.numberOfItems)));
			if (charts.length !== 2 || charts.some(lines => lines.length !== 2 || lines.some(n => n !== history.length)))
				missing.push('download and upload curves with all history samples');
			const edgeCases = await page.evaluate(async () => {
				const sx = await L.require('swanix.common');
				return ['rx_mbps', 'tx_mbps'].every(key => {
					const empty = sx.historyChart([], key);
					const sample = {}; sample[key] = { wan1: 0 };
					const single = sx.historyChart([sample], key);
					const zero = sx.historyChart([sample, sample], key);
					sample[key].wan1 = 0.2;
					const low = sx.historyChart([sample, sample], key);
					return !empty.querySelector('svg') && empty.textContent.includes('Waiting for samples') &&
						single.querySelector('circle') && single.textContent.includes('peak 0.0 Mbps') &&
						zero.querySelector('polyline').points.numberOfItems === 2 && zero.textContent.includes('peak 0.0 Mbps') &&
						low.textContent.includes('peak 0.2 Mbps');
				});
			});
			if (!edgeCases) missing.push('empty, single-sample, zero and low-traffic chart states');
			const totals = await page.evaluate(async () => {
				const sx = await L.require('swanix.common');
				return ['rx_mbps', 'tx_mbps'].every(key => {
					const points = [{ [key]: { wan1: 5, wan2: 5 } }, { [key]: { wan1: 20, wan2: 5 } }, { [key]: { wan1: 1, wan2: 20 } }];
					const before = JSON.stringify(points);
					const chart = sx.historyChart(points, key, 'total');
					const line = chart.querySelector('polyline');
					return chart.querySelectorAll('polyline').length === 1 && line.points.numberOfItems === 3 &&
						line.points[0].y > line.points[1].y && line.points[2].y > line.points[1].y &&
						chart.textContent.includes('Total traffic') && chart.textContent.includes('peak 25.0 Mbps') &&
						JSON.stringify(points) === before && !sx.historyChart([], key, 'total').querySelector('svg');
				});
			});
			if (!totals) missing.push('totals summed at each timestamp without changing per-interface history');
			await page.selectOption('#swanix-traffic-mode', 'total');
			assert.equal(await page.locator('#swanix-traffic-charts polyline').count(), 2);
			const oldCharts = await page.locator('#swanix-traffic-charts').elementHandle();
			await page.waitForFunction(el => !el.isConnected, oldCharts, { timeout: 10000 });
			assert.equal(await page.locator('#swanix-traffic-mode').inputValue(), 'total');
			assert.equal(await page.locator('#swanix-traffic-charts polyline').count(), 2);
			await page.screenshot({ path: path.join(outDir, 'swanix.overview.total.png'), fullPage: true });
			await page.selectOption('#swanix-traffic-mode', 'interfaces');
			assert.equal(await page.locator('#swanix-traffic-charts polyline').count(), 4);
			console.log('ok   total and per-interface traffic modes, per-sample sums and polling selection');
		}
		if (!titleRe.test(text)) missing.push(String(titleRe));
		if (errors.length || missing.length) { failed++; console.error(`FAIL ${view}\n  errors: ${JSON.stringify(errors)}\n  missing: ${JSON.stringify(missing)}\n  text: ${text.slice(0, 300).replace(/\n/g, ' | ')}`); }
		else console.log(`ok   ${view}`);
		if (view === 'swanix.interface_history') {
			assert.equal(await page.locator('.swanix-interface-history svg').count(), 3);
			await page.selectOption('#swanix-history-interface', 'wan2/4');
			assert.match(await page.locator('.swanix-interface-history').innerText(), /97\.9%/);
			assert.equal(await page.locator('.swanix-interface-history svg[aria-label="State history"] rect').count(), 720);
			const gaps = await page.evaluate(async () => {
				const s = await L.require('swanix.stability');
				const rows = [
					{ at: 30000, sample: { state: 'healthy', rtt_ms: 10, probe_failure_rate: 0, down_events: 1 } },
					{ at: 60000, sample: { state: 'down', rtt_ms: null, probe_failure_rate: 1, down_events: 1 } },
					{ at: 90000, sample: { state: 'healthy', rtt_ms: 20, probe_failure_rate: 0, down_events: 0 } }
				];
				const graph = s.chart(rows, 'rtt_ms', 0, 120000, 30000);
				return { lines: graph.querySelectorAll('line[stroke="#1565c0"]').length, dots: graph.querySelectorAll('circle').length, stats: s.summary(rows), empty: s.summary([]) };
			});
			assert.equal(gaps.lines, 0); assert.equal(gaps.dots, 2);
			assert.deepEqual(gaps.stats, { healthy: '66.7%', downs: 2, meanRTT: '15.0 ms', maxRTT: '20.0 ms', maxFailure: '100.0%' });
			assert.equal(gaps.empty.healthy, '-');
			await page.selectOption('#swanix-history-interface', 'wan2/6');
			assert.equal(await page.locator('.swanix-interface-history svg').count(), 1);
			assert.match(await page.locator('.swanix-interface-history').innerText(), /No fresh probe results/);
			await page.selectOption('#swanix-history-interface', 'wan1/6');
			assert.match(await page.locator('.swanix-interface-history').innerText(), /45\.0 ms/);
			console.log('ok   interface history isolates families, counts short outages and leaves probe gaps');
			let override = { interval_seconds: 30, retention_seconds: 21600, points: [] };
			await page.route('**/ubus/**', async route => {
				const requests = route.request().postDataJSON();
				const list = Array.isArray(requests) ? requests : [requests];
				if (!list.some(r => r.params && r.params[1] === 'swanix' && r.params[2] === 'interface_history')) return route.continue();
				const response = await route.fetch();
				const reply = await response.json();
				const replies = Array.isArray(reply) ? reply : [reply];
				list.forEach((r, i) => {
					if (r.params && r.params[1] === 'swanix' && r.params[2] === 'interface_history') replies[i].result = [0, override];
				});
				await route.fulfill({ response, json: Array.isArray(reply) ? replies : replies[0] });
			});
			await page.reload();
			await page.waitForFunction(() => document.querySelector('#view').innerText.includes('No interface history yet'));
			assert.equal(await page.locator('.swanix-interface-history svg').count(), 0);
			override = { ok: false, text: 'Interface history backend unavailable' };
			await page.reload();
			await page.waitForFunction(() => document.querySelector('#view').innerText.includes('Interface history backend unavailable'));
			console.log('ok   interface history empty state and backend errors');
		}
		if (['swanix.policies', 'swanix.wans', 'swanix.health'].includes(view)) {
			await page.evaluate(async () => {
				const uci = await L.require('uci');
				uci.add('swanix', 'policy', 'CPE');
				uci.set('swanix', 'CPE', 'member', ['wan1']);
				uci.set('swanix', 'CPE', 'affinity', 'connection');
			});
			// Exercise the actual Add buttons: collisions across types and within
			// a type must leave the complete UCI cache unchanged and open no modal.
			const creates = page.locator('#view .cbi-section-create');
			for (let i = 0; i < await creates.count(); i++) {
				for (const name of ['CPE', 'balanced', 'wan1', 'health_default', 'global']) {
					const before = await page.evaluate(async () => JSON.stringify((await L.require('uci')).sections('swanix')));
					const input = creates.nth(i).locator('input');
					await input.fill(name);
					await input.dispatchEvent('keyup');
					await creates.nth(i).locator('button').click();
					await page.waitForFunction(() => document.body.innerText.includes('already in use'));
					assert.equal(await page.evaluate(async () => JSON.stringify((await L.require('uci')).sections('swanix'))), before);
					assert.equal(await page.locator('body.modal-overlay-active').count(), 0);
				}
			}
			console.log(`ok   ${view} duplicate names preserve configuration`);
			if (view === 'swanix.policies') {
				const input = creates.nth(1).locator('input');
				await input.fill('CPE_rule');
				await input.dispatchEvent('keyup');
				await creates.nth(1).locator('button').click();
				await page.waitForSelector('body.modal-overlay-active');
				assert.equal(await page.evaluate(async () => (await L.require('uci')).get('swanix', 'CPE_rule', '.type')), 'rule');
				assert.equal(await page.evaluate(async () => (await L.require('uci')).get('swanix', 'CPE', '.type')), 'policy');
				console.log('ok   CPE_rule opens normally and preserves CPE policy');
			}
		}
		await page.close();
	}
	for (const [view] of pages) {
		if (view === 'swanix.proxy' && status.profile !== 'full') continue;
		const page = await browser.newPage();
		await page.goto(`${base}/?view=${view}&lang=zh_Hans`);
		await page.waitForFunction(() => document.querySelector('#view h2'));
		const text = await page.locator('#view').innerText();
		assert.match(text, /[\u4e00-\u9fff]/);
		if (view === 'swanix.policies') {
			for (const label of ['策略与规则', '成员线路', '按连接（默认）', '拒绝连接', '规则']) assert.ok(text.includes(label), label);
			const result = await page.evaluate(async () => {
				const dom = await L.require('dom');
				const map = dom.findClassInstance(document.querySelector('#view .cbi-map'));
				const rule = map.children.find(s => s.sectiontype === 'rule');
				return rule.children.find(o => o.option === 'proto').textvalue('missing');
			});
			assert.equal(result, '任意');
		}
		if (view === 'swanix.overview') assert.ok(text.includes('探测已恢复'));
		if (view === 'swanix.interface_history') {
			assert.ok(text.includes('接口历史')); assert.ok(text.includes('最近 6 小时'));
			await page.setViewportSize({ width: 320, height: 800 });
			await page.selectOption('#swanix-history-interface', 'wan2/4');
			assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true);
			await page.screenshot({ path: path.join(outDir, 'swanix.interface_history.mobile.png'), fullPage: true });
		}
		await page.screenshot({ path: path.join(outDir, view + '.zh_Hans.png'), fullPage: true });
		console.log(`ok   ${view} Chinese translation`);
		await page.close();
	}
	if (process.env.PROBE_MODAL === '1') {
		const page = await browser.newPage();
		await page.goto(`${base}/?view=swanix.health`);
		await page.waitForSelector('#view [data-section-id="health_default"]', { timeout: 15000 });
		await page.locator('#view [data-section-id="health_default"] button:has-text("Edit")').click();
		await page.waitForSelector('.modal');
		console.log((await page.locator('.modal').innerHTML()).replace(/\s+/g, ' ').slice(0, 3000));
		console.log('BUTTONS:', await page.locator('button').allInnerTexts());
	}
	await browser.close(); server.close();
	if (unknown.size) console.log('ubus calls answered with {} (not part of swanix): ' + [...unknown].join(', '));
	process.exit(failed ? 1 : 0);
})();
