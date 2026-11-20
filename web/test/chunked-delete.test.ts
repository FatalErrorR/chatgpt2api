import assert from "node:assert/strict";
import { test } from "node:test";

import { ChunkedDeleteError, deleteInChunks, type DeleteProgress } from "../src/lib/chunked-delete.ts";

const paths = Array.from({ length: 5 }, (_, i) => `img${i}.png`);

test("deletes in chunks and reports progress after each one", async () => {
  const calls: string[][] = [];
  const seen: DeleteProgress[] = [];
  const removed = await deleteInChunks(paths, async (chunk) => {
    calls.push(chunk);
    return { removed: chunk.length };
  }, (p) => seen.push(p), 2);
  assert.equal(removed, 5);
  assert.deepEqual(calls, [["img0.png", "img1.png"], ["img2.png", "img3.png"], ["img4.png"]]);
  assert.deepEqual(seen.map((p) => p.done), [0, 2, 4, 5]);
  assert.ok(seen.every((p) => p.total === 5));
});

test("counts what the server actually removed", async () => {
  const removed = await deleteInChunks(paths, async (chunk) => ({ removed: chunk.length - 1 }), () => {}, 3);
  assert.equal(removed, 3);
});

test("stops at a failing chunk and keeps the count so far", async () => {
  let call = 0;
  await assert.rejects(
    deleteInChunks(paths, async (chunk) => {
      call += 1;
      if (call === 2) throw new Error("boom");
      return { removed: chunk.length };
    }, () => {}, 2),
    (error: unknown) => error instanceof ChunkedDeleteError && error.removed === 2 && error.message === "boom",
  );
  assert.equal(call, 2);
});

test("empty list makes no request", async () => {
  let called = false;
  assert.equal(await deleteInChunks([], async () => { called = true; return { removed: 0 }; }, () => {}), 0);
  assert.equal(called, false);
});
