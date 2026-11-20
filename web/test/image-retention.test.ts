import assert from "node:assert/strict";
import { test } from "node:test";

import { dayBefore, parseRetentionDays, retentionKeepFrom } from "../src/lib/image-retention.ts";

test("keeps the last N calendar days including today", () => {
  assert.equal(retentionKeepFrom("2026-11-20", 15), "2026-11-06");
  assert.equal(retentionKeepFrom("2026-11-20", 1), "2026-11-20");
  assert.equal(retentionKeepFrom("2026-03-01", 2), "2026-02-28");
  assert.equal(retentionKeepFrom("2027-01-02", 3), "2026-12-31");
});

test("day before crosses month and year", () => {
  assert.equal(dayBefore("2026-11-06"), "2026-11-05");
  assert.equal(dayBefore("2026-01-01"), "2025-12-31");
});

test("only whole days from 1 to 3650 are accepted", () => {
  assert.equal(parseRetentionDays(" 15 "), 15);
  for (const bad of ["", "0", "-1", "1.5", "abc", "3651"]) assert.equal(parseRetentionDays(bad), null, bad);
});
