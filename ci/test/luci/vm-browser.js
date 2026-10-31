#!/usr/bin/env node
/* Drive the real LuCI (uhttpd + rpcd + the installed luci-app-swanix) of a running OpenWrt VM
 * with a headless browser: log in, open every swanix page, check it renders live data from the
 * real backend without JavaScript or HTTP errors. Used as the host step of ci/test/vm.
 *
 * Usage: node vm-browser.js <base url> <out dir> [lite|full]
 */
'use strict';
const path = require('path');
const fs = require('fs');
const { chromium } = require('playwright');

const [base, outDir, profile = 'lite'] = process.argv.slice(2);
if (!base || !outDir) { console.error('usage: vm-browser.js <base url> <out dir> [lite|full]'); process.exit(2); }
fs.mkdirSync(outDir, { recursive: true });

/* [page, texts that must be visible, DOM selectors that must exist]; section names of a grid are
 * rendered by the theme, so they are checked through data-section-id rather than innerText */
const pages = [
	['overview', ['Multi-WAN overview', 'wan1', 'wan2', 'healthy'], []],
	['wans', ['Lines (WAN)', 'Download (Mbps)', '500', '300'], ['[data-section-id="wan1"]', '[data-section-id="wan2"]']],
	['policies', ['Policies and rules', 'Member lines', 'Stickiness'], ['[data-section-id="balanced"]']],
	['health', ['Health checks', 'Targets', 'Interval (s)'], ['[data-section-id="health_default"]']],
	['ipv6', ['IPv6 mode'], []],
	['system', ['System / Diagnostics', 'System check (doctor)', 'tool:nft'], []],
];
if (profile === 'full') pages.push(['proxy', ['Proxy (Full build)'], []]);

(async () => {
	const browser = await chromium.launch();
	const ctx = await browser.newContext({ viewport: { width: 1366, height: 900 } });
	const page = await ctx.newPage();
	const errors = [];
	page.on('pageerror', e => errors.push('pageerror: ' + e.message));
	page.on('console', m => { if (m.type() === 'error' && !/favicon/.test(m.text())) errors.push('console: ' + m.text()); });
	page.on('response', r => { if (r.status() >= 400 && !/favicon/.test(r.url())) errors.push(`HTTP ${r.status()}: ${r.url()}`); });

	let failed = 0;
	const fail = (what, detail) => { failed++; console.error(`FAIL ${what}: ${detail}`); };

	// login (root; the VM sets the password "vmtest")
	await page.goto(`${base}/cgi-bin/luci/admin/network/swanix/overview`);
	if (await page.locator('input[name="luci_username"]').count()) {
		await page.fill('input[name="luci_username"]', 'root');
		await page.fill('input[name="luci_password"]', 'vmtest');
		await Promise.all([page.waitForNavigation({ waitUntil: 'load' }), page.locator('input[name="luci_password"]').press('Enter')]);
	}

	// the menu entry exists under Network, and the proxy page only on Full
	await page.goto(`${base}/cgi-bin/luci/admin/network/swanix`);
	await page.waitForTimeout(1500);
	const nav = await page.locator('body').innerText();
	if (!/Multi-WAN/i.test(nav)) fail('menu', 'no "Multi-WAN (swanix)" entry under Network');
	if (profile !== 'full' && /\bProxy\b/.test(await page.locator('ul.nav, #mainmenu, .main-left, nav').allInnerTexts().then(a => a.join('\n')).catch(() => ''))) fail('menu', 'Proxy entry visible on a Lite build');

	for (const [name, needles, selectors] of pages) {
		errors.length = 0;
		await page.goto(`${base}/cgi-bin/luci/admin/network/swanix/${name}`);
		try {
			await page.waitForFunction(() => { const v = document.querySelector('#view'); return v && !v.querySelector('.spinning') && v.children.length > 0; }, null, { timeout: 40000 });
		} catch (e) { errors.push('view never finished loading'); }
		await page.waitForTimeout(1500);
		const text = await page.locator('#view').innerText().catch(() => '');
		await page.screenshot({ path: path.join(outDir, `${name}.png`), fullPage: true });
		const missing = needles.filter(n => !text.includes(n));
		for (const sel of selectors) if (!(await page.locator('#view ' + sel).count())) missing.push(sel);
		if (errors.length || missing.length) fail(name, `errors=${JSON.stringify(errors)} missing=${JSON.stringify(missing)} text=${text.slice(0, 200).replace(/\n/g, ' | ')}`);
		else console.log(`ok   ${name}`);
	}
	/* Save & Apply through the real UI: edit a health setting, save, confirm the takeover window */
	if (process.env.SWANIX_UI_APPLY === '1') {
		errors.length = 0;
		let step = 'open health page';
		try {
			await page.goto(`${base}/cgi-bin/luci/admin/network/swanix/health`);
			await page.waitForSelector('#view [data-section-id="health_default"]', { timeout: 40000 });
			step = 'open the edit dialog';
			await page.locator('#view [data-section-id="health_default"] button:has-text("Edit")').click();
			await page.waitForSelector('.modal', { timeout: 10000 });
			step = 'edit the interval field';
			const field = page.locator('.modal [data-name="interval"] input');
			await field.waitFor({ timeout: 10000 });
			if (await field.isDisabled()) throw new Error('the field is disabled (no write access for this session?)');
			await field.fill('7');
			step = 'save the dialog';
			await page.locator('.modal button:has-text("Save")').click();
			await page.waitForTimeout(500);
			step = 'press Save & Apply';
			/* the footer's split button is a dropdown (ul/li), not a <button>, in LuCI 24.10/25.12 */
			await page.locator('.cbi-page-actions :visible:text-is("Save & Apply")').first().click({ timeout: 15000 });
			step = 'wait for the confirmation dialog';
			await page.waitForSelector('.modal:has-text("Confirm the new multi-WAN setup")', { timeout: 150000 });
			await page.screenshot({ path: path.join(outDir, 'apply-confirm.png') });
			step = 'confirm';
			await page.locator('.modal button:has-text("Confirm")').click();
			await page.waitForLoadState('load', { timeout: 30000 });
			await page.waitForTimeout(1500);
			console.log('ok   save & apply -> confirm');
		} catch (e) {
			await page.screenshot({ path: path.join(outDir, 'apply-failed.png') }).catch(() => {});
			const footer = await page.locator('.cbi-page-actions').first().evaluate(n => n.outerHTML).catch(() => '(no footer)');
			console.log('footer: ' + footer.slice(0, 1200));
			const modal = await page.locator('.modal').first().innerText().catch(() => '(no modal)');
			const notes = await page.locator('.alert-message, #notices, .notifications').allInnerTexts().catch(() => []);
			fail('save & apply', `while trying to ${step}: ${String(e).split('\n').slice(0, 12).join(' / ')}; errors=${JSON.stringify(errors)}; modal=${JSON.stringify(modal.slice(0, 300))}; notes=${JSON.stringify(notes).slice(0, 400)}`);
		}
	}
	await browser.close();
	process.exit(failed ? 1 : 0);
})();
