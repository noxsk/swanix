'use strict';
'require view';
'require form';
'require uci';
'require ui';
'require swanix/common as sx';

return view.extend({
	load: function() { return Promise.all([uci.load('swanix'), uci.load('network')]); },

	render: function() {
		var m = new form.Map('swanix', _('IPv6'),
			_('IPv4 and IPv6 are scheduled independently: a line can be healthy for one family and down for the other.'));
		var s = m.section(form.NamedSection, 'ipv6', 'ipv6', _('IPv6 mode'));
		var o = s.option(form.ListValue, 'mode', _('Mode'));
		o.value('native', _('native — no translation; the source prefix decides the line'));
		o.value('nat66', _('nat66 — per-connection balancing with translation (authorised prefixes only)'));
		o.value('unmanaged', _('unmanaged — swanix does not touch IPv6'));
		var ula = 'fd00::/64';
		uci.sections('network', 'globals').forEach(function(g) { if (g.ula_prefix) ula = g.ula_prefix; });
		o = s.option(form.DynamicList, 'nat66_source', _('NAT66 source prefixes'), _('Prefilled from the local ULA prefix, or fd00::/64. Use the actual LAN prefix; only these prefixes are translated. This does not assign addresses to the LAN.'));
		o.default = [ula]; o.rmempty = false;
		o.datatype = 'cidr6'; o.depends('mode', 'nat66');
		o = s.option(form.ListValue, 'nat66_policy', _('NAT66 egress policy'), _('Used when no traffic rule matches. A policy with one line fixes the egress; multiple lines enable balancing and failover.'));
		o.value('', _('Follow the global default policy'));
		uci.sections('swanix', 'policy').forEach(function(p) { o.value(p['.name']); });
		o.depends('mode', 'nat66');
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
