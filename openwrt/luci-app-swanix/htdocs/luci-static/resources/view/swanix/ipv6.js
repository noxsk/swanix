'use strict';
'require view';
'require form';
'require uci';
'require ui';
'require swanix/common as sx';

return view.extend({
	load: function() { return uci.load('swanix'); },

	render: function() {
		var m = new form.Map('swanix', _('IPv6'),
			_('IPv4 and IPv6 are scheduled independently: a line can be healthy for one family and down for the other.'));
		var s = m.section(form.NamedSection, 'ipv6', 'ipv6', _('IPv6 mode'));
		var o = s.option(form.ListValue, 'mode', _('Mode'));
		o.value('native', _('native — no translation; the source prefix decides the line'));
		o.value('nat66', _('nat66 — per-connection balancing with translation (authorised prefixes only)'));
		o.value('unmanaged', _('unmanaged — swanix does not touch IPv6'));
		o = s.option(form.DynamicList, 'nat66_source', _('NAT66 source prefixes'), _('Required for nat66. Only these LAN prefixes are translated.'));
		o.datatype = 'cidr6'; o.depends('mode', 'nat66');
		o = s.option(form.Flag, 'nat66_router_traffic', _('Also translate the router\'s own connections'));
		o.depends('mode', 'nat66');
		return m.render();
	},

	handleSaveApply: function(ev) {
		return this.handleSave(ev).then(function() { return sx.applyWithConfirm(uci); }).catch(function(e) {
			ui.addNotification(null, E('p', e.message), 'danger');
		});
	}
});
