'use strict';
'require view';
'require form';
'require uci';
'require network';
'require ui';
'require swanix/common as sx';

return view.extend({
	load: function() { return Promise.all([ network.getNetworks(), uci.load('swanix') ]); },

	render: function(data) {
		var m = new form.Map('swanix', _('Lines (WAN)'),
			_('Capacities must be the real line rates you can use. They steer the scheduler; the interface link speed is deliberately not used. The example values shipped with the package are not your line rates.'));

		var g = m.section(form.NamedSection, 'global', 'global', _('Global'));
		var o = g.option(form.Flag, 'enabled', _('Enable'), _('Installed disabled. Enabling does not take over the network until you apply.'));
		o.rmempty = false;
		o = g.option(form.ListValue, 'startup_mode', _('Mode'));
		o.value('observe', _('observe — read only, no kernel changes'));
		o.value('manage', _('manage — take over routing (confirm window applies)'));
		o = g.option(form.DynamicList, 'lan_network', _('Managed LAN networks'));
		data[0].forEach(function(n) { o.value(n.getName()); });
		g.option(form.Flag, 'manage_router_traffic', _('Manage the router\'s own outgoing traffic'));
		o = g.option(form.ListValue, 'default_policy', _('Default policy'));
		uci.sections('swanix', 'policy').forEach(function(p) { o.value(p['.name']); });

		var s = m.section(form.GridSection, 'wan', _('Lines'));
		s.addremove = true;
		s.anonymous = false;
		s.nodescriptions = true;
		o = s.option(form.Flag, 'enabled', _('Enabled')); o.default = '1'; o.editable = true;
		o = s.option(form.Value, 'id', _('ID'), _('Stable number 1–14. Never changes when lines are reordered.'));
		o.datatype = 'range(1,14)'; o.rmempty = false;
		o = s.option(form.ListValue, 'network', _('Interface'));
		data[0].forEach(function(n) { o.value(n.getName()); });
		o.rmempty = false;
		o = s.option(form.Value, 'ipv6_network', _('IPv6 interface'), _('auto, none, or a logical interface'));
		o.default = 'auto'; o.modalonly = true;
		o = s.option(form.Value, 'download_mbps', _('Download (Mbps)')); o.datatype = 'ufloat'; o.rmempty = false;
		o = s.option(form.Value, 'upload_mbps', _('Upload (Mbps)')); o.datatype = 'ufloat'; o.rmempty = false;
		o = s.option(form.Value, 'preference', _('Preference'), _('Relative weight, default 100. Not a capacity.'));
		o.datatype = 'range(1,1000)'; o.default = '100'; o.modalonly = true;
		o = s.option(form.ListValue, 'role', _('Role'));
		o.value('active', _('active — shares traffic')); o.value('backup', _('backup — only when no active line is usable'));
		o = s.option(form.ListValue, 'traffic_bias', _('Traffic bias'));
		o.value('balanced', _('balanced')); o.value('download', _('download')); o.value('upload', _('upload'));
		o.modalonly = true;
		o = s.option(form.ListValue, 'health', _('Health profile'));
		uci.sections('swanix', 'health').forEach(function(h) { o.value(h['.name']); });
		o.modalonly = true;

		return m.render();
	},

	handleSaveApply: function(ev) {
		return this.handleSave(ev).then(function() { return sx.applyWithConfirm(uci); }).catch(function(e) {
			ui.addNotification(null, E('p', e.message), 'danger');
		});
	}
});
