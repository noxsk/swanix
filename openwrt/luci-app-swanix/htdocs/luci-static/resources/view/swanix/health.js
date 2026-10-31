'use strict';
'require view';
'require form';
'require uci';
'require ui';
'require swanix/common as sx';

return view.extend({
	load: function() { return uci.load('swanix'); },

	render: function() {
		var m = new form.Map('swanix', _('Health checks'),
			_('Probe targets must be IP addresses so name resolution can never be answered through another line. The failure rate shown on the overview is the probe failure ratio, not link packet loss.'));
		var s = m.section(form.GridSection, 'health', _('Profiles'));
		s.addremove = true; s.anonymous = false; s.nodescriptions = true;
		var o = s.option(form.DynamicList, 'target', _('Targets'), _('ip:port, [ipv6]:port or http(s)://ip/path'));
		o.rmempty = false;
		o = s.option(form.Value, 'interval', _('Interval (s)')); o.datatype = 'min(1)'; o.default = '5';
		o = s.option(form.Value, 'timeout', _('Timeout (s)')); o.datatype = 'min(1)'; o.default = '3'; o.modalonly = true;
		o = s.option(form.Value, 'successes_required', _('Targets that must answer per round'), _('Independent targets, not consecutive rounds.')); o.datatype = 'min(1)'; o.default = '1';
		o = s.option(form.Value, 'down_rounds', _('Failed rounds before down')); o.datatype = 'min(1)'; o.default = '3';
		o = s.option(form.Value, 'up_rounds', _('Good rounds before recovery')); o.datatype = 'min(1)'; o.default = '3';
		o = s.option(form.Value, 'recover_ramp', _('Recovery ramp (s)'), _('Time over which a recovered line is brought back to full share.')); o.datatype = 'min(0)'; o.default = '30';
		return m.render();
	},

	handleSaveApply: function(ev) {
		return this.handleSave(ev).then(function() { return sx.applyWithConfirm(uci); }).catch(function(e) {
			ui.addNotification(null, E('p', e.message), 'danger');
		});
	}
});
