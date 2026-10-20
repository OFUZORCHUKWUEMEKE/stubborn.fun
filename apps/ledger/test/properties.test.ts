import fc from "fast-check";
import { describe, expect, it } from "vitest";
import { balanceOf, expectBalanced, expectOk, fund, service, uid } from "./helpers";
import ledgerSource from "../src/ledger-do.ts?raw";

describe("settlement properties", () => {
  it("for any two stakes, the pot is paid out exactly and nothing leaks", async () => {
    const stake = fc.integer({ min: 1, max: 1_000_000_000 }); // up to ₦10m each
    await fc.assert(
      fc.asyncProperty(stake, stake, fc.boolean(), async (stakeA, stakeB, aWins) => {
        const [a, b] = [uid("a"), uid("b")];
        await fund(a, stakeA);
        await fund(b, stakeB);
        const gameId = uid("game");
        expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: a, amount: stakeA }));
        expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: b, amount: stakeB }));

        const winner = aWins ? a : b;
        const r = expectOk(await service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: winner }));

        const pot = stakeA + stakeB;
        expect(r.pot).toBe(pot);
        expect(r.payout + r.fee).toBe(pot);
        expect(BigInt(r.fee)).toBe((BigInt(pot) * 1000n) / 10_000n);
        expect(await balanceOf(winner)).toBe(r.payout);
        expect(await balanceOf(aWins ? b : a)).toBe(0);
        expect(expectOk(await service.escrow({ currency: "NGN", gameId })).balance).toBe(0);
      }),
      { numRuns: 150 },
    );
    await expectBalanced();
  });
});

describe("the no-await rule", () => {
  // Every money operation must run as one synchronous transaction. An
  // await inside one would let another request run in between and bring
  // back the check-then-act races the single-threaded design removes.
  // The operation methods are not async, so TypeScript already rejects an
  // await in them; this test also catches an async callback slipped into
  // transactionSync or a helper.
  it("the ledger Durable Object contains no async code at all", () => {
    // Comments explain the rule using these very words, so strip them.
    const code = ledgerSource.replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/.*$/gm, "");
    expect(code.length).toBeGreaterThan(1000);
    expect(code).not.toMatch(/\bawait\b/);
    expect(code).not.toMatch(/\basync\b/);
    expect(code).not.toMatch(/\.then\(/);
  });
});
