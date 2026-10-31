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

const [luciRes, outDir] = process.argv.slice(2);
if (!luciRes || !outDir) { console.error('usage: render.js <luci-base resources> <out dir>'); process.exit(2); }
const repo = path.resolve(__dirname, '../../..');
const appRes = path.join(repo, 'openwrt/luci-app-swanix/htdocs/luci-static/resources');
fs.mkdirSync(outDir, { recursive: true });

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
	case 'session.access': return { 'access-group': { 'luci-app-swanix': ['read', 'write'] } };
	case 'uci.get': return args.config === 'swanix' ? { values: uciValues } : { values: {} };
	case 'uci.state': case 'uci.changes': return {};
	case 'swanix.status': return status;
	case 'swanix.history': return history;
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
	const browser = await chromium.launch();
	const pages = [
		['swanix.overview', /Multi-WAN overview/, ['wan1', 'recovering', '62%', 'probes recovered']],
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
		catch (e) { errors.push('view never finished loading'); }
		await page.waitForTimeout(600);
		const text = await page.locator('#view').innerText().catch(() => '');
		await page.screenshot({ path: path.join(outDir, view + '.png'), fullPage: true });
		const missing = needles.filter(n => !text.includes(n));
		if (!titleRe.test(text)) missing.push(String(titleRe));
		if (errors.length || missing.length) { failed++; console.error(`FAIL ${view}\n  errors: ${JSON.stringify(errors)}\n  missing: ${JSON.stringify(missing)}\n  text: ${text.slice(0, 300).replace(/\n/g, ' | ')}`); }
		else console.log(`ok   ${view}`);
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
