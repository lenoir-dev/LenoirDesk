const {test} = require('node:test');
const assert = require('node:assert/strict');
const {preciseUTC, permitted} = require('../internal/api/assets/historical-picker.js');

test('UTC fields preserve seconds and do not use the browser timezone', () => {
  assert.equal(preciseUTC('2026-03-08', '02:30:47'), '2026-03-08T02:30:47Z');
  assert.equal(preciseUTC('2026-11-01', '01:30:59'), '2026-11-01T01:30:59Z');
  assert.equal(preciseUTC('2024-02-29', '23:59:58'), '2024-02-29T23:59:58Z');
  assert.equal(preciseUTC('2025-02-29', '23:59:58'), null);
  assert.equal(preciseUTC('2026-04-31', '12:00:00'), null);
  assert.equal(preciseUTC('2026-12-01', '24:00:00'), null);
});

test('client window agrees with bounded server contract at edges', () => {
  const now = Date.parse('2026-11-02T12:00:00Z');
  assert.equal(permitted({day:'2026-10-04',clock:'12:00:00'}, {day:'2026-11-02',clock:'11:58:00'}, now), true);
  assert.equal(permitted({day:'2026-10-04',clock:'11:59:59'}, {day:'2026-11-02',clock:'11:58:00'}, now), false);
  assert.equal(permitted({day:'2026-11-02',clock:'11:58:00'}, {day:'2026-11-02',clock:'11:58:00'}, now), false);
  assert.equal(permitted({day:'2026-11-02',clock:'11:00:00'}, {day:'2026-11-02',clock:'11:58:01'}, now), false);
});
