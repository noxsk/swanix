'use strict';
'require view';
'require form';
'require uci';
'require ui';
'require swanix/common as sx';

return view.extend({
	load: function() { return uci.load('swanix'); },

	render: function() {
		var m = new form.Map('swanix', _('Policies and rules'),
			_('A policy is a pool of lines. Every new connection picks a line once and keeps it. Unmatched traffic uses the default policy.'));

		var s = m.section(form.GridSection, 'policy', _('Policies'));
		s.addremove = true; s.anonymous = false; s.nodescriptions = true;
		var o = s.option(form.DynamicList, 'member', _('Member lines'));
		uci.sections('swanix', 'wan').forEach(function(w) { o.value(w['.name']); });
		o.rmempty = false;
		o = s.option(form.ListValue, 'affinity', _('Stickiness'), _('Per connection spreads best. Sticky modes keep a client on its line across connections while that line is usable, which reduces balancing for that client.'));
		o.value('connection', _('per connection (default)'));
		o.value('source', _('per source address'));
		o.value('source_destination', _('per source + destination address'));
		o = s.option(form.Value, 'affinity_timeout', _('Stickiness timeout (s)'));
		o.datatype = 'range(10,86400)'; o.default = '300'; o.modalonly = true;
		o = s.option(form.Value, 'saturation_threshold', _('Saturation threshold (%)'),
			_('Optional. When a line carries this share of its configured bandwidth and another member is below it, new connections go to the unsaturated lines; the line takes new connections again once it is 10 points below. 0 or empty: off.'));
		o.datatype = 'or(range(0,0),range(50,99))'; o.placeholder = '0'; o.modalonly = true;
		o = s.option(form.ListValue, 'unavailable_action', _('When no line is usable'));
		o.value('reject', _('reject (never silently use the main table)'));

		var r = m.section(form.GridSection, 'rule', _('Rules'));
		r.addremove = true; r.anonymous = false; r.nodescriptions = true;
		o = r.option(form.Flag, 'enabled', _('Enabled')); o.default = '1'; o.editable = true;
		o = r.option(form.Value, 'priority', _('Priority'), _('Lower numbers match first.')); o.datatype = 'range(1,9999)'; o.default = '100';
		o = r.option(form.DynamicList, 'src', _('Source')); o.datatype = 'or(ipaddr,cidr)'; o.modalonly = true;
		o = r.option(form.DynamicList, 'dest', _('Destination')); o.datatype = 'or(ipaddr,cidr)';
		o = r.option(form.ListValue, 'proto', _('Protocol'));
		o.value('', _('any')); o.value('tcp', 'TCP'); o.value('udp', 'UDP'); o.value('icmp', 'ICMP'); o.value('icmpv6', 'ICMPv6');
		o = r.option(form.DynamicList, 'dest_port', _('Destination ports'), _('80 or 8000-8100; needs TCP or UDP')); o.modalonly = true;
		o = r.option(form.Flag, 'local', _('Router-originated traffic only')); o.modalonly = true;
		o = r.option(form.ListValue, 'policy', _('Policy'));
		uci.sections('swanix', 'policy').forEach(function(p) { o.value(p['.name']); });

		return m.render();
	},

	handleSaveApply: function(ev) {
		return this.handleSave(ev).then(function() { return sx.applyWithConfirm(uci); }).catch(function(e) {
			ui.addNotification(null, E('p', e.message), 'danger');
		});
	}
});
