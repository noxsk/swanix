'use strict';
'require view';
'require poll';
'require dom';
'require swanix/common as sx';

return view.extend({
	load: function() {
		return Promise.all([ sx.callStatus(), sx.callHistory() ]);
	},

	render: function(data) {
		var root = E('div', { 'class': 'cbi-map' }, [
			E('h2', {}, _('Multi-WAN overview')),
			E('div', { 'class': 'cbi-map-descr' }, _('"New-connection share" is how many new connections the scheduler hands to a line. It is not a bandwidth share: one connection always stays on one line.'))
		]);
		var body = E('div', { 'id': 'swanix-body' });
		root.appendChild(body);

		var refresh = function(st, hist) {
			var nodes = [];
			if (st.error || st.ok === false) {
				nodes.push(E('div', { 'class': 'alert-message warning' }, st.text || st.error || _('The swanix daemon is not running.')));
				dom.content(body, nodes);
				return;
			}
			nodes.push(E('p', {}, [ _('Profile') + ': ', E('strong', {}, st.profile), ' · ', _('Mode') + ': ', E('strong', {}, st.mode),
				' · ', st.managing ? _('managing the kernel data path') : _('observe only: nothing is written to the kernel, health comes from netifd link state') ]));
			(st.problems || []).forEach(function(p) { nodes.push(E('div', { 'class': 'alert-message warning' }, p)); });

			var table = E('table', { 'class': 'table' }, [
				E('tr', { 'class': 'tr table-titles' }, [
					E('th', { 'class': 'th' }, _('Line')), E('th', { 'class': 'th' }, _('Family')), E('th', { 'class': 'th' }, _('State')),
					E('th', { 'class': 'th' }, _('Down / Up now')), E('th', { 'class': 'th' }, _('Capacity')),
					E('th', { 'class': 'th' }, _('Probe RTT (base)')), E('th', { 'class': 'th' }, _('Probe failure rate')),
					E('th', { 'class': 'th' }, _('New-connection share')), E('th', { 'class': 'th' }, _('Reason'))
				])
			]);
			(st.wans || []).forEach(function(w) {
				(w.families || []).forEach(function(f, i) {
					var share = Object.keys(f.new_connection_share || {}).map(function(k) { return k + ' ' + f.new_connection_share[k] + '%'; }).join(', ');
					table.appendChild(E('tr', { 'class': 'tr' }, [
						E('td', { 'class': 'td' }, i == 0 ? w.name + ' (' + (w.device || '?') + ')' : ''),
						E('td', { 'class': 'td' }, 'IPv' + f.family),
						E('td', { 'class': 'td' }, sx.stateBadge(f.state)),
						E('td', { 'class': 'td' }, i == 0 ? sx.fmtMbps(w.rx_mbps) + ' / ' + sx.fmtMbps(w.tx_mbps) : ''),
						E('td', { 'class': 'td' }, i == 0 ? sx.fmtMbps(w.download_capacity_mbps) + ' / ' + sx.fmtMbps(w.upload_capacity_mbps) : ''),
						E('td', { 'class': 'td' }, f.rtt_ms ? f.rtt_ms.toFixed(0) + ' ms (' + f.base_rtt_ms.toFixed(0) + ')' : '-'),
						E('td', { 'class': 'td' }, (f.probe_failure_rate * 100).toFixed(0) + ' %'),
						E('td', { 'class': 'td' }, share || '-'),
						E('td', { 'class': 'td' }, f.reason || '')
					]));
				});
			});
			nodes.push(table);
			if (st.rejected_connections > 0)
				nodes.push(E('p', {}, _('Rejected new connections (no usable line): ') + st.rejected_connections));

			nodes.push(E('h3', {}, _('Download (last hour, 5 s resolution)')));
			nodes.push(sx.historyChart(hist, 'rx_mbps'));
			nodes.push(E('h3', {}, _('Upload')));
			nodes.push(sx.historyChart(hist, 'tx_mbps'));

			nodes.push(E('h3', {}, _('Recent state changes')));
			var tr = (st.recent_transitions || []).slice().reverse().slice(0, 15);
			nodes.push(tr.length ? E('ul', {}, tr.map(function(t) {
				return E('li', {}, [ new Date(t.at).toLocaleTimeString(), ' ', t.wan, ' IPv', t.family, ': ', t.from, ' → ', t.to, ' (', t.reason, ')' ]);
			})) : E('p', { 'class': 'cbi-value-description' }, _('None since start.')));
			dom.content(body, nodes);
		};

		refresh(data[0], data[1]);
		/* 5 s refresh; LuCI's poll pauses automatically while the page is hidden */
		poll.add(function() {
			return Promise.all([ sx.callStatus(), sx.callHistory() ]).then(function(d) { refresh(d[0], d[1]); });
		}, 5);
		return root;
	},

	handleSave: null, handleSaveApply: null, handleReset: null
});
