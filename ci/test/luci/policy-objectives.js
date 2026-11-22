#!/usr/bin/env node
'use strict';
// Focused form-schema regression test; no browser or LuCI runtime required.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const options = {};
const form = {
 Map: function() {
  this.section = (_, type) => ({
   option: (_, name, title, description) => {
    const option = { title, description, choices: [], dependencies: [],
     value: function(value) { this.choices.push(value); },
     depends: function(key, value) { this.dependencies.push([key, value]); }
    };
    options[type + '.' + name] = option;
    return option;
   }
  });
  this.render = () => options;
 }
};
const source = fs.readFileSync(path.resolve(__dirname, '../../../openwrt/luci-app-swanix/htdocs/luci-static/resources/view/swanix/policies.js'), 'utf8');
const page = new Function('view', 'form', 'uci', 'ui', 'sx', '_', source)(
 { extend: x => x }, form, { sections: () => [] }, {},
 { protectSectionNames: () => {}, choiceText: () => {} }, x => x
);
page.render();
const objective = options['policy.objective'];
assert.equal(objective.default, 'capacity');
assert.equal(objective.rmempty, false);
assert.deepEqual(objective.choices, ['capacity', 'latency', 'throughput', 'balanced']);
const size = options['policy.flow_size_kib'];
assert.equal(size.default, '256');
assert.equal(size.datatype, 'range(1,1048576)');
assert.deepEqual(size.dependencies, [['objective', 'balanced']]);
assert.match(size.description, /not automatic flow-size detection/);
const reserve = options['policy.latency_reserve_percent'];
assert.equal(reserve.default, '0');
assert.equal(reserve.datatype, 'range(0,80)');
assert.deepEqual(reserve.dependencies, [['objective', 'throughput']]);
assert.match(reserve.description, /not SQM or rate limiting/);
assert.notEqual(reserve.retain, true, 'hidden reserves must be removed when switching objective');
console.log('Policy objective form tests passed');
