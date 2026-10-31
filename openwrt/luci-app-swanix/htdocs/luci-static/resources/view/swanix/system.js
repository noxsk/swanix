'use strict';
'require view';
'require dom';
'require ui';
'require swanix/common as sx';

return view.extend({
	load: function() { return Promise.all([ sx.callVersion(), sx.callFeatures(), sx.callDoctor() ]); },

	render: function(data) {
		var v = data[0], f = data[1], d = data[2];
		var checks = (d.checks || []).map(function(c) {
			var colour = { ok: '#2e7d32', warn: '#f9a825', fail: '#c62828' }[c.level] || '#757575';
			return E('tr', { 'class': 'tr' }, [
				E('td', { 'class': 'td' }, E('span', { 'style': 'color:' + colour + ';font-weight:bold' }, c.level)),
				E('td', { 'class': 'td' }, c.name), E('td', { 'class': 'td' }, c.detail),
				E('td', { 'class': 'td' }, c.fix || '')
			]);
		});
		var previewBox = E('pre', { 'style': 'max-height:24em;overflow:auto' }, _('Press "Preview" to see the rules that would be installed.'));
		return E('div', { 'class': 'cbi-map' }, [
			E('h2', {}, _('System / Diagnostics')),
			E('h3', {}, _('Build')),
			E('table', { 'class': 'table' }, [
				[ _('Version'), v.version + ' (' + v.profile + ')' ],
				[ _('Fork commit'), v.commit ],
				[ _('Upstream'), 'sing-box ' + v.upstream_version + ' (' + v.upstream_commit + ')' ],
				[ _('Platform'), v.os + '/' + v.arch + ' · ' + v.go_version + ' · cgo=' + v.cgo ],
				[ _('OpenWrt series'), v.openwrt_series || '-' ],
				[ _('Verified scope'), (v.verified_scope || []).join(', ') || _('none recorded for this build') ],
				[ _('Build tags'), (v.build_tags || []).join(', ') ]
			].map(function(r) { return E('tr', { 'class': 'tr' }, [ E('td', { 'class': 'td left', 'width': '25%' }, r[0]), E('td', { 'class': 'td left' }, r[1]) ]); })),
			E('h3', {}, _('System check (doctor)')),
			E('table', { 'class': 'table' }, [ E('tr', { 'class': 'tr table-titles' }, [
				E('th', { 'class': 'th' }, _('Result')), E('th', { 'class': 'th' }, _('Check')), E('th', { 'class': 'th' }, _('Detail')), E('th', { 'class': 'th' }, _('How to fix')) ]) ].concat(checks)),
			E('h3', {}, _('Resource plan')),
			E('div', { 'class': 'cbi-page-actions' }, [ E('button', { 'class': 'btn cbi-button', 'click': function() {
				sx.callPreview().then(function(r) { dom.content(previewBox, r.text); });
			} }, _('Preview')) ]),
			previewBox
		]);
	},

	handleSave: null, handleSaveApply: null, handleReset: null
});
