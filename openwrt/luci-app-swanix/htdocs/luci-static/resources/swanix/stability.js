'use strict';
'require baseclass';

var colours = { healthy: '#2e7d32', degraded: '#f9a825', recovering: '#1565c0', down: '#c62828', unknown: '#757575' };
var svgNS = 'http://www.w3.org/2000/svg';

function svgElement(name, attrs, text) {
	var el = document.createElementNS(svgNS, name);
	Object.keys(attrs || {}).forEach(function(k) { el.setAttribute(k, attrs[k]); });
	if (text != null) el.textContent = text;
	return el;
}

/* Keep real timestamps and missing evidence: a failed probe is never RTT 0. */
function samples(history, wan, family, start, end) {
	var rows = [];
	(history.points || []).forEach(function(p) {
		var at = Date.parse(p.at);
		if (!isFinite(at) || at < start || at > end) return;
		(p.interfaces || []).forEach(function(s) {
			if (s.wan === wan && s.family === family) rows.push({ at: at, sample: s });
		});
	});
	return rows.sort(function(a, b) { return a.at - b.at; });
}

function summary(rows) {
	var healthy = 0, downs = 0, rtts = [], failures = [];
	rows.forEach(function(r) {
		var s = r.sample;
		if (s.state === 'healthy') healthy++;
		downs += s.down_events || 0;
		if (s.rtt_ms != null && isFinite(s.rtt_ms)) rtts.push(s.rtt_ms);
		if (s.probe_failure_rate != null && isFinite(s.probe_failure_rate)) failures.push(s.probe_failure_rate);
	});
	return {
		healthy: rows.length ? (healthy * 100 / rows.length).toFixed(1) + '%' : '-',
		downs: downs,
		meanRTT: rtts.length ? (rtts.reduce(function(a, b) { return a + b; }, 0) / rtts.length).toFixed(1) + ' ms' : '-',
		maxRTT: rtts.length ? Math.max.apply(null, rtts).toFixed(1) + ' ms' : '-',
		maxFailure: failures.length ? (Math.max.apply(null, failures) * 100).toFixed(1) + '%' : '-'
	};
}

/* All charts share a time axis. Each state cell covers one sample interval;
 * missing intervals remain empty and metric paths break at gaps/nulls. */
function chart(rows, key, start, end, interval) {
	var W = Math.max(280, Math.min(720, document.documentElement.clientWidth - 40));
	var H = key === 'state' ? 56 : 150, left = 50, right = 8, top = 12, bottom = H - 28;
	var svg = svgElement('svg', { viewBox: '0 0 ' + W + ' ' + H, role: 'img',
		'aria-label': key === 'state' ? _('State history') : key === 'rtt_ms' ? _('Probe RTT history (ms)') : _('Probe failure history (%)'),
		style: 'display:block;width:100%;max-width:' + W + 'px;background:var(--background-color-medium, #f6f6f6);border-radius:4px' });
	var x = function(t) { return left + (W - left - right) * (t - start) / Math.max(1, end - start); };
	var max = key === 'probe_failure_rate' ? 1 : Math.max(1, rows.reduce(function(m, r) { return Math.max(m, r.sample[key] || 0); }, 0));
	if (key !== 'state') {
		[0, 0.5, 1].forEach(function(f) {
			var y = bottom - (bottom - top) * f;
			svg.appendChild(svgElement('line', { x1: left, x2: W - right, y1: y, y2: y, stroke: 'currentColor', opacity: '.15' }));
			svg.appendChild(svgElement('text', { x: left - 5, y: y + 4, 'text-anchor': 'end', fill: 'currentColor', 'font-size': 11 }, (max * f * (key === 'probe_failure_rate' ? 100 : 1)).toFixed(0)));
		});
	}
	var prev = null;
	rows.forEach(function(r) {
		var s = r.sample, title = new Date(r.at).toLocaleString() + '\n' + _(s.state) + ': ' + _(s.reason || '');
		if (s.down_events) title += '\n' + _('Interruptions') + ': ' + s.down_events;
		if (key === 'state') {
			var cell = svgElement('rect', { x: x(Math.max(start, r.at - interval)), y: top,
				width: Math.max(.6, x(r.at) - x(Math.max(start, r.at - interval))), height: bottom - top,
				fill: colours[s.state] || colours.unknown });
			cell.appendChild(svgElement('title', {}, title));
			svg.appendChild(cell);
			if (s.down_events) svg.appendChild(svgElement('circle', { cx: x(r.at), cy: top, r: 3, fill: colours.down }));
			return;
		}
		var value = s[key];
		if (value == null || !isFinite(value)) { prev = null; return; }
		var y = bottom - (bottom - top) * value / max;
		if (prev && r.at - prev.at <= interval * 1.5)
			svg.appendChild(svgElement('line', { x1: x(prev.at), y1: prev.y, x2: x(r.at), y2: y, stroke: '#1565c0', 'stroke-width': 1.5 }));
		var dot = svgElement('circle', { cx: x(r.at), cy: y, r: 1.5, fill: '#1565c0' });
		dot.appendChild(svgElement('title', {}, title + '\n' + (value * (key === 'probe_failure_rate' ? 100 : 1)).toFixed(1) + (key === 'rtt_ms' ? ' ms' : '%')));
		svg.appendChild(dot);
		prev = { at: r.at, y: y };
	});
	[0, .5, 1].forEach(function(f) {
		var at = start + (end - start) * f;
		svg.appendChild(svgElement('text', { x: x(at), y: H - 6, 'text-anchor': f === 0 ? 'start' : f === 1 ? 'end' : 'middle', fill: 'currentColor', 'font-size': 11 }, new Date(at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })));
	});
	return svg;
}

return baseclass.extend({ samples: samples, summary: summary, chart: chart });
