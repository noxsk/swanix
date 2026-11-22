'use strict';
'require view';
'require poll';
'require rpc';
'require dom';
'require swanix/common as sx';
'require swanix/stability as stability';

var callHistory = rpc.declare({ object: 'swanix', method: 'interface_history', expect: { '': {} }, filter: function(res) {
	if (res.ok === false || !Array.isArray(res.points))
		throw new Error(res.text || _('Interface history is unavailable. Update the swanix daemon and swanix-common together.'));
	return res;
} });

return view.extend({
	load: function() { return callHistory().catch(function(err) { return { error: err.message }; }); },
	render: function(history) {
		var selected = '', root = E('div', { 'class': 'cbi-map' }, [
			E('h2', {}, _('Interface history')),
			E('p', { 'class': 'cbi-map-descr' }, _('Last 6 hours, sampled every 30 seconds. History is kept in memory and cleared on restart.')),
			E('p', { 'class': 'cbi-value-description' }, _('Probe failure rate measures TCP/HTTP probe failures, not packet loss. Observe mode records link state only. Missing or stale probe results appear as gaps.'))
		]);
		var error = E('div'), controls = E('div'), body = E('div', { 'class': 'swanix-interface-history' });
		root.appendChild(error); root.appendChild(controls); root.appendChild(body);
		function refresh(data) {
			history = data;
			if (data.error) {
				dom.content(error, E('div', { 'class': 'alert-message warning' }, _('Could not refresh interface history: ') + data.error));
				dom.content(controls, []); dom.content(body, []);
				return;
			}
			var interfaces = {};
			(history.points || []).forEach(function(p) { (p.interfaces || []).forEach(function(s) { interfaces[s.wan + '/' + s.family] = s; }); });
			var keys = Object.keys(interfaces).sort();
			if (!interfaces[selected]) selected = keys[0] || '';
			if (!selected) {
				dom.content(controls, []);
				dom.content(body, E('p', {}, _('No interface history yet. Waiting for samples.')));
				return;
			}
			var select = E('select', { 'id': 'swanix-history-interface', 'change': function(ev) { selected = ev.target.value; refresh(history); } }, keys.map(function(k) {
				var s = interfaces[k];
				return E('option', { value: k, selected: k === selected ? '' : null }, s.wan + ' / IPv' + s.family);
			}));
			dom.content(controls, E('label', {}, [ _('Interface') + ': ', select ]));
			var s = interfaces[selected], end = Date.parse(history.points[history.points.length - 1].at), start = end - 6 * 3600000;
			var rows = stability.samples(history, s.wan, s.family, start, end), stats = stability.summary(rows);
			var latest = rows[rows.length - 1];
			var nodes = [ E('p', {}, [ s.network + ' (' + (s.device || '?') + ') · ', sx.stateBadge(s.state),
				' · ' + _('Last sample') + ': ' + new Date(latest.at).toLocaleString() ]),
				E('p', { 'class': 'cbi-value-description' }, _('Healthy sample share is the fraction of recorded samples in healthy state, not an uptime guarantee. Interruptions count transitions into down, including short outages between samples.')) ];
			var labels = [_('Healthy sample share'), _('Interruptions'), _('Mean sampled RTT'), _('Peak sampled RTT'), _('Peak probe failure rate')];
			var values = [stats.healthy, stats.downs, stats.meanRTT, stats.maxRTT, stats.maxFailure];
			var statsView = E('div', { style: 'display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:.8em;max-width:900px' }, labels.map(function(label, i) {
				return E('div', {}, [ E('div', { 'class': 'cbi-value-description' }, label), E('strong', { style: 'font-size:1.3em' }, String(values[i])) ]);
			}));
			nodes.push(statsView, E('h3', {}, _('State history')), stability.chart(rows, 'state', start, end, history.interval_seconds * 1000));
			nodes.push(E('p', {}, ['healthy', 'degraded', 'recovering', 'down', 'unknown'].map(function(state) { return E('span', { style: 'display:inline-block;margin:.2em .6em .2em 0' }, [sx.stateBadge(state)]); })));
			nodes.push(E('p', { 'class': 'cbi-value-description' }, _('Red dots mark sample intervals with interruptions.')));
			['rtt_ms', 'probe_failure_rate'].forEach(function(key) {
				nodes.push(E('h3', {}, key === 'rtt_ms' ? _('Probe RTT history (ms)') : _('Probe failure history (%)')));
				nodes.push(rows.some(function(r) { return r.sample[key] != null; }) ? stability.chart(rows, key, start, end, history.interval_seconds * 1000) :
					E('p', { 'class': 'cbi-value-description' }, _('No fresh probe results in this period.')));
			});
			dom.content(body, nodes);
		}
		refresh(history);
		poll.add(function() { return callHistory().then(function(data) { dom.content(error, []); refresh(data); }).catch(function(err) {
			dom.content(error, E('div', { 'class': 'alert-message warning' }, _('Could not refresh interface history: ') + err.message));
		}); }, 30);
		return root;
	},
	handleSave: null, handleSaveApply: null, handleReset: null
});
