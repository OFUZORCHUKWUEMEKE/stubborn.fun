import { describe, expect, it } from "vitest";
import { balanceOf, expectBalanced, expectFailure, expectOk, fund, service, uid } from "./helpers";

describe("idempotency", () => {
  it("replaying an op returns the original result and moves nothing", async () => {
    const user = uid("u");
    const req = { opId: uid("dep"), currency: "NGN" as const, userId: user, amount: 7_000, provider: "paystack" };

    const first = await service.deposit(req);
    expectOk(first);
    expect(first.ok && first.replayed).toBe(false);

    for (let i = 0; i < 3; i++) {
      const again = await service.deposit(req);
      expectOk(again);
      expect(again.ok && again.replayed).toBe(true);
      expect(again.ok && again.value).toEqual(first.ok && first.value);
    }

    expect(await balanceOf(user)).toBe(7_000);
    await expectBalanced();
  });

  it("reusing an op id for a different request is a conflict, not a silent no-op", async () => {
    const user = uid("u");
    const opId = uid("dep");
    expectOk(await service.deposit({ opId, currency: "NGN", userId: user, amount: 1_000, provider: "paystack" }));

    // Same op id, different amount: a caller bug that must be loud.
    expectFailure(
      await service.deposit({ opId, currency: "NGN", userId: user, amount: 9_000, provider: "paystack" }),
      "IDEMPOTENCY_CONFLICT",
    );
    // Same op id, different operation entirely.
    expectFailure(
      await service.lockStake({ opId, currency: "NGN", gameId: uid("g"), userId: user, amount: 1 }),
      "IDEMPOTENCY_CONFLICT",
    );
    expect(await balanceOf(user)).toBe(1_000);
  });

  it("a failed op is not recorded, so it can be retried under the same id", async () => {
    const user = uid("u");
    const gameId = uid("game");
    const opId = uid("stk");
    const stake = { opId, currency: "NGN" as const, gameId, userId: user, amount: 5_000 };

    expectFailure(await service.lockStake(stake), "INSUFFICIENT_FUNDS");
    await fund(user, 5_000);

    const retried = await service.lockStake(stake);
    expectOk(retried);
    expect(retried.ok && retried.replayed).toBe(false);
    expect(await balanceOf(user)).toBe(0);
    await expectBalanced();
  });

  it("the same op fired concurrently applies exactly once", async () => {
    const user = uid("u");
    const req = { opId: uid("dep"), currency: "NGN" as const, userId: user, amount: 100, provider: "paystack" };

    const results = await Promise.all(Array.from({ length: 50 }, () => service.deposit(req)));

    const fresh = results.filter((r) => r.ok && !r.replayed);
    const replays = results.filter((r) => r.ok && r.replayed);
    expect(fresh).toHaveLength(1);
    expect(replays).toHaveLength(49);
    expect(await balanceOf(user)).toBe(100);
    await expectBalanced();
  });

  it("settlement replays return the same payout without paying twice", async () => {
    const [a, b] = [uid("a"), uid("b")];
    await fund(a, 1_000);
    await fund(b, 1_000);
    const gameId = uid("game");
    expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: a, amount: 1_000 }));
    expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: b, amount: 1_000 }));

    const settle = { opId: `settle:${gameId}`, currency: "NGN" as const, gameId, winnerUserId: a };
    const results = await Promise.all(Array.from({ length: 20 }, () => service.settleGame(settle)));

    expect(results.every((r) => r.ok)).toBe(true);
    expect(results.filter((r) => r.ok && !r.replayed)).toHaveLength(1);
    expect(await balanceOf(a)).toBe(1_800);
    await expectBalanced();
  });
});
