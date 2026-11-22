'use strict';
'require baseclass';
'require rpc';
'require ui';

/* Shared helpers for the swanix views. Everything is native LuCI JS; the
 * history chart is a small inline SVG, no charting library. */

var callStatus = rpc.declare({ object: 'swanix', method: 'status', expect: {} });
var callHistory = rpc.declare({ object: 'swanix', method: 'history', expect: { points: [] } });
var callFeatures = rpc.declare({ object: 'swanix', method: 'features', expect: {} });
var callVersion = rpc.declare({ object: 'swanix', method: 'version', expect: {} });
var callDoctor = rpc.declare({ object: 'swanix', method: 'doctor', expect: {} });
var callPreview = rpc.declare({ object: 'swanix', method: 'preview', expect: {} });
var callCheck = rpc.declare({ object: 'swanix', method: 'check', expect: {} });
var callPrepare = rpc.declare({ object: 'swanix', method: 'prepare', expect: {} });
var callRestore = rpc.declare({ object: 'swanix', method: 'restore', expect: {} });
var callApply = rpc.declare({ object: 'swanix', method: 'apply', params: [ 'timeout' ], expect: {} });
var callConfirm = rpc.declare({ object: 'swanix', method: 'confirm', expect: {} });
var callRollback = rpc.declare({ object: 'swanix', method: 'rollback', expect: {} });

/* UCI section names are unique across all types in the package. LuCI's
 * default add handler does not check this before replacing the cached entry. */
function protectSectionNames(section) {
	var handleAdd = section.handleAdd;
	section.handleAdd = function(ev, name) {
		var config = this.uciconfig || this.map.config;
		if (name && this.map.data.get(config, name) != null) {
			ui.addNotification(null, E('p',
				_('The name "%s" is already in use. Names must be unique across lines, policies, rules and health profiles. Choose another name, for example "%s".').format(name, name + '_' + this.sectiontype)), 'danger');
			return Promise.resolve();
		}
		return handleAdd.call(this, ev, name);
	};
}

/* GridSection otherwise prints the stored enum instead of its choice label. */
function choiceText(section_id) {
	var value = this.cfgvalue(section_id);
	if (value == null) value = this.default;
	if (value == null) value = '';
	var index = this.keylist.indexOf(value);
	return index >= 0 ? this.vallist[index] : '%h'.format(value);
}

function fmtMbps(v) {
	return (v == null) ? '-' : (v >= 100 ? v.toFixed(0) : v.toFixed(1)) + ' Mbps';
}

function stateBadge(state) {
	var colour = { healthy: '#2e7d32', degraded: '#f9a825', recovering: '#1565c0', down: '#c62828', unknown: '#757575' }[state] || '#757575';
	return E('span', { 'style': 'display:inline-block;padding:0 .6em;border-radius:1em;color:#fff;background:' + colour }, [ _(state) ]);
}

/* points: [{at, rx_mbps:{wan:v}, tx_mbps:{wan:v}}]; one line per WAN,
 * or one total computed across WANs separately at each sample time. */
function historyChart(points, key, mode) {
	if (mode === 'total') {
		points = (points || []).map(function(p) {
			var values = p[key] || {}, names = Object.keys(values), sample = {};
			sample[key] = {};
			if (names.length) sample[key].total = names.reduce(function(sum, n) { return sum + values[n]; }, 0);
			return sample;
		});
	}
	var W = 640, H = 120, pad = 4;
	var names = {};
	var max = 0;
	(points || []).forEach(function(p) {
		Object.keys(p[key] || {}).forEach(function(n) { names[n] = true; max = Math.max(max, p[key][n]); });
	});
	if (!Object.keys(names).length)
		return E('p', { 'class': 'cbi-value-description' }, _('No traffic history yet. Waiting for samples.'));
	var scale = Math.max(1, max);
	var palette = [ '#1565c0', '#ef6c00', '#2e7d32', '#6a1b9a', '#00838f', '#ad1457' ];
	var svgNS = 'http://www.w3.org/2000/svg';
	var svg = document.createElementNS(svgNS, 'svg');
	svg.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
	svg.setAttribute('style', 'width:100%;max-width:' + W + 'px;height:auto;background:var(--background-color-medium, #f6f6f6);border-radius:4px');
	Object.keys(names).sort().forEach(function(n, i) {
		var pts = (points || []).map(function(p, idx) {
			var x = pad + (W - 2 * pad) * idx / Math.max(1, points.length - 1);
			var y = H - pad - (H - 2 * pad) * ((p[key] || {})[n] || 0) / scale;
			return x.toFixed(1) + ',' + y.toFixed(1);
		}).join(' ');
		var line = document.createElementNS(svgNS, 'polyline');
		line.setAttribute('points', pts);
		line.setAttribute('fill', 'none');
		line.setAttribute('stroke', palette[i % palette.length]);
		line.setAttribute('stroke-width', '1.5');
		svg.appendChild(line);
		if (points.length === 1) {
			var dot = document.createElementNS(svgNS, 'circle');
			var xy = pts.split(',');
			dot.setAttribute('cx', xy[0]);
			dot.setAttribute('cy', xy[1]);
			dot.setAttribute('r', '2');
			dot.setAttribute('fill', palette[i % palette.length]);
			svg.appendChild(dot);
		}
	});
	var legend = Object.keys(names).sort().map(function(n, i) {
		return E('span', { 'style': 'margin-right:1em' }, [
			E('span', { 'style': 'display:inline-block;width:.8em;height:.8em;margin-right:.3em;background:' + palette[i % palette.length] }),
			mode === 'total' ? _('Total traffic') : n
		]);
	});
	return E('div', {}, [ svg, E('div', { 'style': 'margin-top:.3em' }, legend.concat([ E('span', { 'class': 'cbi-value-description' }, [ _('peak') + ' ' + fmtMbps(max) ]) ])) ]);
}

/* Save & apply with the confirmation window. The caller has already staged
 * UCI changes; this commits them, applies, and offers confirm/rollback. */
function applyWithConfirm(uci, timeout) {
	return callPrepare().then(function() {
		return uci.save().then(function() { return uci.apply(false); });
	}).then(function() {
		return callCheck();
	}).then(function(chk) {
		if (!chk.ok) {
			/* the change is already committed: put the previous configuration back */
			return callRestore().then(function() { uci.unload('swanix'); throw new Error(chk.text); });
		}
		return callApply(timeout || 90);
	}).then(function(res) {
		if (!res.ok) throw new Error(res.text);
		if (res.confirm === false) {
			/* observe mode or disabled: nothing was taken over, so there is no window to confirm */
			ui.addNotification(null, E('p', res.text), 'info');
			return;
		}
		var left = timeout || 90;
		var banner = E('div', { 'class': 'alert-message warning' }, [
			E('p', {}, _('Switched to swanix. Confirm within the time limit or the previous setup is restored automatically.')),
			E('p', {}, [ E('strong', { 'id': 'swanix-countdown' }, left + ' s') ]),
			E('div', { 'class': 'right' }, [
				E('button', { 'class': 'btn cbi-button-negative', 'click': function() {
					callRollback().then(function() { ui.hideModal(); window.location.reload(); });
				} }, _('Roll back now')), ' ',
				E('button', { 'class': 'btn cbi-button-positive', 'click': function() {
					callConfirm().then(function(r) {
						ui.hideModal();
						if (!r.ok) ui.addNotification(null, E('p', r.text), 'danger');
						else window.location.reload();
					});
				} }, _('Confirm'))
			])
		]);
		ui.showModal(_('Confirm the new multi-WAN setup'), [ banner ]);
		var timer = window.setInterval(function() {
			left -= 1;
			var el = document.getElementById('swanix-countdown');
			if (el) el.textContent = left + ' s';
			if (left <= 0) { window.clearInterval(timer); window.location.reload(); }
		}, 1000);
	});
}

return baseclass.extend({
	callStatus: callStatus, callHistory: callHistory, callFeatures: callFeatures, callVersion: callVersion,
	callDoctor: callDoctor, callPreview: callPreview, callCheck: callCheck, callPrepare: callPrepare, callRestore: callRestore,
	callApply: callApply, callConfirm: callConfirm, callRollback: callRollback,
	protectSectionNames: protectSectionNames, choiceText: choiceText,
	fmtMbps: fmtMbps, stateBadge: stateBadge, historyChart: historyChart, applyWithConfirm: applyWithConfirm
});
