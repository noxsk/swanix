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
		var chartMode = 'interfaces';
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
			nodes.push(E('p', {}, [ _('Profile') + ': ', E('strong', {}, st.profile), ' · ', _('Mode') + ': ', E('strong', {}, _(st.mode)),
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
						E('td', { 'class': 'td' }, _(f.reason || ''))
					]));
				});
			});
			nodes.push(table);
			if (st.rejected_connections > 0)
				nodes.push(E('p', {}, _('Rejected new connections (no usable line): ') + st.rejected_connections));

			var charts = E('div', { 'id': 'swanix-traffic-charts' });
			var drawCharts = function() {
				dom.content(charts, [
					E('h3', {}, _('Download (last hour, 5 s resolution)')), sx.historyChart(hist, 'rx_mbps', chartMode),
					E('h3', {}, _('Upload')), sx.historyChart(hist, 'tx_mbps', chartMode)
				]);
			};
			nodes.push(E('label', {}, [ _('Traffic chart mode') + ': ', E('select', {
				'id': 'swanix-traffic-mode', 'change': function(ev) { chartMode = ev.target.value; drawCharts(); }
			}, [
				E('option', { value: 'total', selected: chartMode === 'total' ? '' : null }, _('Total traffic')),
				E('option', { value: 'interfaces', selected: chartMode === 'interfaces' ? '' : null }, _('By interface'))
			]) ]));
			drawCharts();
			nodes.push(charts);

			nodes.push(E('h3', {}, _('Recent state changes')));
			var tr = (st.recent_transitions || []).slice().reverse().slice(0, 15);
			nodes.push(tr.length ? E('ul', {}, tr.map(function(t) {
				return E('li', {}, [ new Date(t.at).toLocaleTimeString(), ' ', t.wan, ' IPv', t.family, ': ', _(t.from), ' → ', _(t.to), ' (', _(t.reason), ')' ]);
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
