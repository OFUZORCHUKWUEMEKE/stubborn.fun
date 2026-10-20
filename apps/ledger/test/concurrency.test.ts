import { describe, expect, it } from "vitest";
import { balanceOf, expectBalanced, expectOk, fund, service, uid } from "./helpers";

describe("concurrency", () => {
  it("200 concurrent stakes from one wallet cannot overdraw it", async () => {
    const user = uid("u");
    const stake = 100;
    await fund(user, 50 * stake); // enough for exactly 50

    const results = await Promise.all(
      Array.from({ length: 200 }, () =>
        service.lockStake({ opId: uid("stk"), currency: "NGN", gameId: uid("game"), userId: user, amount: stake }),
      ),
    );

    const ok = results.filter((r) => r.ok);
    const refused = results.filter((r) => !r.ok);
    expect(ok).toHaveLength(50);
    expect(refused).toHaveLength(150);
    for (const r of refused) {
      expect(!r.ok && r.error.code).toBe("INSUFFICIENT_FUNDS");
    }
    expect(await balanceOf(user)).toBe(0);
    await expectBalanced();
  });

  it("settle and refund racing on one game: exactly one wins", async () => {
    for (let round = 0; round < 10; round++) {
      const [a, b] = [uid("a"), uid("b")];
      await fund(a, 1_000);
      await fund(b, 1_000);
      const gameId = uid("game");
      expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: a, amount: 1_000 }));
      expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: b, amount: 1_000 }));

      const [settled, refunded] = await Promise.all([
        service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: a }),
        service.refundGame({ opId: uid("ref"), currency: "NGN", gameId }),
      ]);

      expect([settled.ok, refunded.ok].filter(Boolean)).toHaveLength(1);
      const loser = settled.ok ? refunded : settled;
      expect(!loser.ok && loser.error.code).toBe("ESCROW_CLOSED");

      const escrow = expectOk(await service.escrow({ currency: "NGN", gameId }));
      expect(escrow.balance).toBe(0);
      const total = (await balanceOf(a)) + (await balanceOf(b));
      // Settled: 2000 pot minus 200 fee. Refunded: both stakes back.
      expect(total).toBe(settled.ok ? 1_800 : 2_000);
    }
    await expectBalanced();
  });

  it("many games settling concurrently keep every invariant", async () => {
    const games = await Promise.all(
      Array.from({ length: 40 }, async () => {
        const [a, b] = [uid("a"), uid("b")];
        await fund(a, 2_500);
        await fund(b, 2_500);
        const gameId = uid("game");
        expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: a, amount: 2_500 }));
        expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: b, amount: 2_500 }));
        return { gameId, a, b };
      }),
    );

    const results = await Promise.all(
      games.map((g, i) =>
        i % 3 === 0
          ? service.refundGame({ opId: uid("ref"), currency: "NGN", gameId: g.gameId })
          : service.settleGame({ opId: uid("set"), currency: "NGN", gameId: g.gameId, winnerUserId: g.a }),
      ),
    );

    expect(results.every((r) => r.ok)).toBe(true);
    await expectBalanced();
  });
});
