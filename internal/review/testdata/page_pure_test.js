// Runs the page's pure functions (between the /*pure:start*/ and /*pure:end*/
// markers in page.html) under node. Invoked by TestPagePureFunctions.
const fs = require("fs");
const page = fs.readFileSync(process.argv[2], "utf8");
const m = page.match(/\/\*pure:start\*\/([\s\S]*?)\/\*pure:end\*\//);
if (!m) { console.error("no pure block"); process.exit(1); }
const { compareScale, rejectChange, groupOrder, keepLabels } =
  new Function(m[1] + "; return { compareScale, rejectChange, groupOrder, keepLabels };")();
let failed = 0;
const check = (cond, msg) => { if (!cond) { console.error("FAIL: " + msg); failed++; } };

// Two 1024 px subject crops in a 1440 px window: tiles with padding must fit one row.
{
  const w = 1440 - 32, n = 2, pad = 18, gap = 12;
  const k = compareScale(w, n, 1024, pad, gap);
  check(n * (1024 * k + pad) + gap * (n - 1) <= w, "two tiles overflow the row: k=" + k);
  check(compareScale(5000, 2, 1024, pad, gap) === 1, "never upscales");
}

// A single rejected change restores what the frame showed before.
{
  const labels = { "A.DNG": { label: "keep", stars: 3 } };
  const pending = [{ file: "A.DNG", label: "cull", stars: 3, prev: { label: "keep", stars: 3 } }];
  const reverted = rejectChange(pending, labels);
  check(reverted && labels["A.DNG"].label === "keep" && pending.length === 0, "single revert");
}

// Two queued changes for one frame, both rejected: the original comes back, and only
// the last refusal says it reverted.
{
  const labels = { "A.DNG": { label: "keep", stars: 3 } };
  const pending = [
    { file: "A.DNG", label: "keep", stars: 0, prev: {} },
    { file: "A.DNG", label: "keep", stars: 3, prev: { label: "keep", stars: 0 } },
  ];
  check(!rejectChange(pending, labels), "first refusal must not claim a revert");
  check(rejectChange(pending, labels), "second refusal reverts");
  check(!labels["A.DNG"], "the original (no label) must come back, got " + JSON.stringify(labels["A.DNG"]));
}

// A queued entry from an older page version has no prev: nothing to restore.
{
  const labels = { "A.DNG": { label: "cull", stars: 0 } };
  const pending = [{ file: "A.DNG", label: "cull", stars: 0 }];
  check(!rejectChange(pending, labels) && labels["A.DNG"].label === "cull" && pending.length === 0, "legacy entry");
}
// groupOrder keeps card order but pulls each set's shown members together at the
// position of its first shown member; frames in no set (or a set of one) stay put.
{
  const g = (id, size) => ({ group: { id, size } });
  const cards = [g(1, 2), {}, g(2, 3), g(1, 2), g(2, 3), {}, g(3, 1), g(2, 3)];
  const got = groupOrder([0, 1, 2, 3, 4, 5, 6, 7], cards);
  check(JSON.stringify(got) === JSON.stringify([0, 3, 1, 2, 4, 7, 5, 6]), "groupOrder " + JSON.stringify(got));
  const filtered = groupOrder([1, 3, 4], cards); // set 1's first member filtered out
  check(JSON.stringify(filtered) === JSON.stringify([1, 3, 4]), "groupOrder filtered " + JSON.stringify(filtered));
}

// keepLabels: the top n by the model's rank keep, the other ranked members cull;
// unranked members (rank 0: the model culled them for sharpness) are left alone.
{
  const members = [{ file: "A", rank: 2 }, { file: "B", rank: 1 }, { file: "C", rank: 3 }, { file: "D", rank: 0 }];
  const two = keepLabels(members, 2);
  check(two.A === "keep" && two.B === "keep" && two.C === "cull" && !("D" in two), "keep 2 " + JSON.stringify(two));
  const none = keepLabels(members, 0);
  check(none.A === "cull" && none.B === "cull" && none.C === "cull", "keep 0 " + JSON.stringify(none));
  const all = keepLabels(members, 9);
  check(all.A === "keep" && all.B === "keep" && all.C === "keep", "keep all " + JSON.stringify(all));
}
process.exit(failed ? 1 : 0);
