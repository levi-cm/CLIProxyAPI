const {test} = require('node:test');
const assert = require('node:assert/strict');
const ui = require('./assets/account-policy.js');

test('expiry states never invent countdowns for missing detail or null expiry', () => {
  const now = Date.parse('2026-10-04T16:00:00Z');
  assert.equal(ui.creditState({details_known:false,expires_at:null},now), 'Expiry unknown');
  assert.equal(ui.creditState({details_known:true,expires_at:null},now), 'Does not expire');
  assert.equal(ui.creditState({details_known:true,expires_at:'2026-10-04T15:00:00Z'},now), 'Expired');
  assert.equal(ui.countdown('2026-10-04T16:00:45Z', now), '45s');
  assert.equal(ui.countdown('2026-10-04T19:14:00Z', now), '3h 14m');
});

test('Berlin renders offset at each provider instant while fixed GMT+2 preserves UTC', () => {
  assert.match(ui.instant('2026-10-05T04:18:00Z','Europe/Berlin'), /06:18.*GMT\+2/);
  assert.match(ui.instant('2026-10-22T20:27:00Z','Europe/Berlin'), /22:27.*GMT\+2/);
  assert.match(ui.instant('2026-10-29T17:48:00Z','Europe/Berlin'), /18:48.*GMT\+1/);
  assert.match(ui.instant('2026-10-29T17:48:00Z','GMT+2'), /19:48.*GMT\+2/);
});

test('manual schedules require an explicit UTC offset or Z', () => {
  assert.throws(() => ui.scheduleInstant('2026-10-29T19:00:00'), /offset/);
  assert.equal(ui.scheduleInstant('2026-10-29T19:00:00+01:00'), '2026-10-29T18:00:00.000Z');
});

test('priority uses only fresh structured evidence and ignores unknown credits', () => {
  const now=Date.parse('2026-10-04T16:00:00Z');
  const settings={freshness_seconds:120,expiry_guard_seconds:600,automation:'auto_expiring',credit_types:['weekly']};
  const account={eligible:true,observed_at:'2026-10-04T16:00:00Z',inventory_observed_at:'2026-10-04T16:00:00Z',buckets:[{scope:'weekly',duration_seconds:604800,reset_at:'2026-10-11T16:00:00Z'}],credits:[{id:'b1',type:'weekly',status:'available',details_known:true,expires_at:'2026-10-05T16:00:00Z'}]};
  assert.equal(ui.priority(account,settings,now).deadline,'2026-10-05T15:50:00.000Z');
  assert.equal(ui.priority({...account,observed_at:'2026-10-04T15:00:00Z'},settings,now).deadline,null);
});
