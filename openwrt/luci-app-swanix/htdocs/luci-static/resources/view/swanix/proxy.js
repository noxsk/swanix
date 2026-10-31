'use strict';
'require view';
'require form';
'require uci';
'require ui';
'require swanix/common as sx';

return view.extend({
	load: function() { return Promise.all([ uci.load('swanix'), sx.callFeatures() ]); },

	render: function(data) {
		var m = new form.Map('swanix', _('Proxy (Full build)'),
			_('The proxy configuration itself lives in /etc/swanix/proxy.json and is never rewritten by this page. Set wan_policy on an outbound there to let it pick a line per connection.'));
		var s = m.section(form.NamedSection, 'proxy', 'proxy', _('Proxy'));
		var o = s.option(form.Flag, 'enabled', _('Enable the proxy runtime'));
		o.rmempty = false;
		o = s.option(form.ListValue, 'transparent_mode', _('Transparent takeover'),
			_('With "TUN" the tun inbound in proxy.json (auto_route, without auto_redirect) takes the LAN traffic; swanix checks its rule priorities, routing table and marks against its own, and outbounds with wan_policy pick the line per connection.'));
		o.value('off', _('Off'));
		o.value('tun', _('TUN'));
		o.default = 'off';
		o = s.option(form.Value, 'config_file', _('Proxy configuration file'));
		o.default = '/etc/swanix/proxy.json';
		o = s.option(form.Flag, 'interrupt_on_down', _('Close connections on a failed line'),
			_('When a line goes down, close the proxy connections that were dialled through it, so long-lived sessions (mux, HTTP/2, QUIC) reconnect over a healthy line at once instead of waiting for their own timeouts.'));
		o.default = '1';
		o.rmempty = false;
		var f = data[1];
		var feat = E('details', {}, [ E('summary', {}, _('Compiled-in proxy features') + ' (' + (f.proxy_features || []).length + ')'),
			E('p', { 'class': 'cbi-value-description' }, (f.proxy_features || []).join(', ')) ]);
		return m.render().then(function(node) { node.appendChild(feat); return node; });
	},

	handleSaveApply: function(ev) {
		return this.handleSave(ev).then(function() { return sx.applyWithConfirm(uci); }).catch(function(e) {
			ui.addNotification(null, E('p', e.message), 'danger');
		});
	}
});
