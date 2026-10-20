import { runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { Currency } from "@stubborn/money";
import { balanceOf, expectBalanced, expectFailure, expectOk, fund, ledger, service, uid } from "./helpers";

/** Reads a non-user account (house, clearing, pending) straight from SQLite. */
async function accountBalance(ref: string, currency: Currency = "NGN"): Promise<number> {
  return runInDurableObject(ledger(currency), (_instance, state) => {
    const row = state.storage.sql
      .exec<{ balance: number }>("SELECT balance FROM accounts WHERE ref = ? AND currency = ?", ref, currency)
      .toArray()[0];
    return row?.balance ?? 0;
  });
}

/** Stakes both players into a fresh game and returns its id. */
async function stakedGame(a: string, b: string, stakeA: number, stakeB = stakeA): Promise<string> {
  const gameId = uid("game");
  expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: a, amount: stakeA }));
  expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: b, amount: stakeB }));
  return gameId;
}

describe("deposit", () => {
  it("credits the user and debits the provider's clearing account", async () => {
    const user = uid("u");
    const before = await accountBalance("clearing:paystack");

    const result = expectOk(
      await service.deposit({ opId: uid("dep"), currency: "NGN", userId: user, amount: 25_000, provider: "paystack" }),
    );

    expect(result.userBalance).toBe(25_000);
    expect(await balanceOf(user)).toBe(25_000);
    expect(await accountBalance("clearing:paystack")).toBe(before - 25_000);
    await expectBalanced();
  });
});

describe("lockStake", () => {
  it("moves the stake from the wallet into the game's escrow", async () => {
    const user = uid("u");
    await fund(user, 10_000);
    const gameId = uid("game");

    const result = expectOk(
      await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: user, amount: 4_000 }),
    );

    expect(result).toEqual({ userBalance: 6_000, escrowBalance: 4_000 });
    const escrow = expectOk(await service.escrow({ currency: "NGN", gameId }));
    expect(escrow).toMatchObject({ status: "open", balance: 4_000, feeBps: 1000 });
    expect(escrow.stakes).toEqual([{ userId: user, amount: 4_000 }]);
    await expectBalanced();
  });

  it("refuses an overdraft and leaves no trace", async () => {
    const user = uid("u");
    await fund(user, 1_000);
    const gameId = uid("game");

    expectFailure(
      await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: user, amount: 1_001 }),
      "INSUFFICIENT_FUNDS",
    );

    expect(await balanceOf(user)).toBe(1_000);
    // The escrow the failed stake would have opened was rolled back too.
    expectFailure(await service.escrow({ currency: "NGN", gameId }), "ESCROW_NOT_FOUND");
    await expectBalanced();
  });

  it("refuses a second stake by the same player in the same game", async () => {
    const user = uid("u");
    await fund(user, 10_000);
    const gameId = uid("game");
    expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: user, amount: 1_000 }));

    expectFailure(
      await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: user, amount: 1_000 }),
      "ALREADY_STAKED",
    );
    expect(await balanceOf(user)).toBe(9_000);
  });
});

describe("settleGame", () => {
  it("pays the winner the pot less the fee, and the fee to the house", async () => {
    const [a, b] = [uid("a"), uid("b")];
    await fund(a, 10_000);
    await fund(b, 10_000);
    const gameId = await stakedGame(a, b, 5_000);
    const houseBefore = await accountBalance("house:fees");

    const result = expectOk(
      await service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: a }),
    );

    expect(result).toEqual({ pot: 10_000, payout: 9_000, fee: 1_000, feeBps: 1000, winnerUserId: a });
    expect(await balanceOf(a)).toBe(5_000 + 9_000);
    expect(await balanceOf(b)).toBe(5_000);
    expect(await accountBalance("house:fees")).toBe(houseBefore + 1_000);
    const escrow = expectOk(await service.escrow({ currency: "NGN", gameId }));
    expect(escrow).toMatchObject({ status: "settled", balance: 0 });
    await expectBalanced();
  });

  it("is exact when the fee truncates, with payout + fee === pot", async () => {
    const [a, b] = [uid("a"), uid("b")];
    await fund(a, 333);
    await fund(b, 333);
    const gameId = await stakedGame(a, b, 333);

    const result = expectOk(await service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: b }));

    // 10% of 666 is 66.6: the house takes 66, the winner keeps the fraction.
    expect(result).toMatchObject({ pot: 666, fee: 66, payout: 600 });
    expect(result.payout + result.fee).toBe(result.pot);
    await expectBalanced();
  });

  it("posts nothing to the house when the fee rounds to zero", async () => {
    const [a, b] = [uid("a"), uid("b")];
    await fund(a, 1);
    await fund(b, 1);
    const gameId = await stakedGame(a, b, 1);
    const houseBefore = await accountBalance("house:fees");

    const result = expectOk(await service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: a }));

    expect(result).toMatchObject({ pot: 2, fee: 0, payout: 2 });
    expect(await accountBalance("house:fees")).toBe(houseBefore);
    await expectBalanced();
  });

  it("handles unequal stakes: the winner takes the whole pot less the fee", async () => {
    const [a, b] = [uid("a"), uid("b")];
    await fund(a, 7_000);
    await fund(b, 3_000);
    const gameId = await stakedGame(a, b, 7_000, 3_000);

    const result = expectOk(await service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: b }));
    expect(result).toMatchObject({ pot: 10_000, payout: 9_000, fee: 1_000 });
    expect(await balanceOf(b)).toBe(9_000);
    expect(await balanceOf(a)).toBe(0);
    await expectBalanced();
  });

  it("refuses a winner who did not stake", async () => {
    const [a, b] = [uid("a"), uid("b")];
    await fund(a, 1_000);
    await fund(b, 1_000);
    const gameId = await stakedGame(a, b, 1_000);

    expectFailure(
      await service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: uid("stranger") }),
      "NOT_A_PARTICIPANT",
    );
    expect(expectOk(await service.escrow({ currency: "NGN", gameId })).status).toBe("open");
  });

  it("refuses an unknown game", async () => {
    expectFailure(
      await service.settleGame({ opId: uid("set"), currency: "NGN", gameId: uid("nogame"), winnerUserId: "x" }),
      "ESCROW_NOT_FOUND",
    );
  });

  it("closes the game: no second settlement, refund, or stake", async () => {
    const [a, b, c] = [uid("a"), uid("b"), uid("c")];
    await fund(a, 1_000);
    await fund(b, 1_000);
    await fund(c, 1_000);
    const gameId = await stakedGame(a, b, 1_000);
    expectOk(await service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: a }));

    expectFailure(
      await service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: b }),
      "ESCROW_CLOSED",
    );
    expectFailure(await service.refundGame({ opId: uid("ref"), currency: "NGN", gameId }), "ESCROW_CLOSED");
    expectFailure(
      await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: c, amount: 1_000 }),
      "ESCROW_CLOSED",
    );
    expect(await balanceOf(c)).toBe(1_000);
    await expectBalanced();
  });
});

describe("refundGame", () => {
  it("returns every stake to its owner exactly", async () => {
    const players = [uid("a"), uid("b"), uid("c")];
    const stakes = [1_234, 5_678, 9];
    const gameId = uid("game");
    for (const [i, p] of players.entries()) {
      await fund(p, stakes[i]!);
      expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: p, amount: stakes[i]! }));
    }

    const result = expectOk(await service.refundGame({ opId: uid("ref"), currency: "NGN", gameId }));

    expect(result.refunds).toHaveLength(3);
    for (const [i, p] of players.entries()) {
      expect(await balanceOf(p)).toBe(stakes[i]);
    }
    const escrow = expectOk(await service.escrow({ currency: "NGN", gameId }));
    expect(escrow).toMatchObject({ status: "refunded", balance: 0 });
    await expectBalanced();
  });
});

describe("withdrawals", () => {
  async function held(amount: number) {
    const user = uid("u");
    await fund(user, 10_000);
    const withdrawalId = uid("wd");
    expectOk(
      await service.holdWithdrawal({ opId: uid("hold"), currency: "NGN", withdrawalId, userId: user, amount, provider: "paystack" }),
    );
    return { user, withdrawalId };
  }

  it("hold then complete: the money leaves through the provider's clearing account", async () => {
    const clearingBefore = await accountBalance("clearing:paystack");
    const { user, withdrawalId } = await held(4_000);
    expect(await balanceOf(user)).toBe(6_000);

    expectOk(await service.completeWithdrawal({ opId: uid("done"), currency: "NGN", withdrawalId }));

    expect(await balanceOf(user)).toBe(6_000);
    // The deposit took 10_000 from clearing; the payout returned 4_000.
    expect(await accountBalance("clearing:paystack")).toBe(clearingBefore - 10_000 + 4_000);
    await expectBalanced();
  });

  it("hold then reverse: the money goes back to the user", async () => {
    const { user, withdrawalId } = await held(4_000);

    expectOk(await service.reverseWithdrawal({ opId: uid("rev"), currency: "NGN", withdrawalId }));

    expect(await balanceOf(user)).toBe(10_000);
    await expectBalanced();
  });

  it("a withdrawal closes exactly once", async () => {
    const { withdrawalId } = await held(1_000);
    expectOk(await service.completeWithdrawal({ opId: uid("done"), currency: "NGN", withdrawalId }));

    expectFailure(
      await service.completeWithdrawal({ opId: uid("done"), currency: "NGN", withdrawalId }),
      "WITHDRAWAL_CLOSED",
    );
    expectFailure(
      await service.reverseWithdrawal({ opId: uid("rev"), currency: "NGN", withdrawalId }),
      "WITHDRAWAL_CLOSED",
    );
    await expectBalanced();
  });

  it("refuses an overdraft, a duplicate id, and an unknown id", async () => {
    const user = uid("u");
    await fund(user, 500);
    const withdrawalId = uid("wd");
    expectFailure(
      await service.holdWithdrawal({ opId: uid("hold"), currency: "NGN", withdrawalId, userId: user, amount: 501, provider: "paystack" }),
      "INSUFFICIENT_FUNDS",
    );

    // The failed hold left nothing behind, so the same id is still usable.
    expectOk(
      await service.holdWithdrawal({ opId: uid("hold"), currency: "NGN", withdrawalId, userId: user, amount: 500, provider: "paystack" }),
    );
    expectFailure(
      await service.holdWithdrawal({ opId: uid("hold"), currency: "NGN", withdrawalId, userId: user, amount: 1, provider: "paystack" }),
      "WITHDRAWAL_EXISTS",
    );
    expectFailure(
      await service.completeWithdrawal({ opId: uid("done"), currency: "NGN", withdrawalId: uid("nowd") }),
      "WITHDRAWAL_NOT_FOUND",
    );
    await expectBalanced();
  });
});

describe("input validation", () => {
  const user = "valid-user";

  it.each([
    ["zero", 0],
    ["negative", -100],
    ["fractional", 1.5],
    ["NaN", Number.NaN],
    ["beyond 2^53", 2 ** 60],
  ])("rejects a %s amount", async (_label, amount) => {
    expectFailure(
      await service.deposit({ opId: uid("dep"), currency: "NGN", userId: user, amount, provider: "paystack" }),
      "INVALID_INPUT",
    );
  });

  it.each([
    ["empty", ""],
    ["containing a colon", "user:admin"],
    ["containing a space", "bad id"],
    ["too long", "a".repeat(200)],
  ])("rejects an id %s", async (_label, badId) => {
    expectFailure(
      await service.deposit({ opId: uid("dep"), currency: "NGN", userId: badId, amount: 100, provider: "paystack" }),
      "INVALID_INPUT",
    );
  });

  it("rejects a malformed op id and provider", async () => {
    expectFailure(
      await service.deposit({ opId: "", currency: "NGN", userId: user, amount: 100, provider: "paystack" }),
      "INVALID_INPUT",
    );
    expectFailure(
      await service.deposit({ opId: uid("dep"), currency: "NGN", userId: user, amount: 100, provider: "PayStack!" }),
      "INVALID_INPUT",
    );
  });

  it("rejects an unknown currency at the entrypoint", async () => {
    const result = await service.deposit({
      opId: uid("dep"),
      currency: "USD" as Currency,
      userId: user,
      amount: 100,
      provider: "paystack",
    });
    expectFailure(result, "INVALID_INPUT");
  });

  it("writes nothing for invalid input", async () => {
    const before = await service.reconcile("NGN");
    await service.deposit({ opId: uid("dep"), currency: "NGN", userId: user, amount: -5, provider: "paystack" });
    const after = await service.reconcile("NGN");
    expect(after.checked.entries).toBe(before.checked.entries);
    expect(after.checked.ops).toBe(before.checked.ops);
  });
});

describe("currency isolation", () => {
  it("keeps play money and real money in separate ledgers", async () => {
    const user = uid("u");
    await fund(user, 50_000, "NGN_PLAY");

    expect(await balanceOf(user, "NGN_PLAY")).toBe(50_000);
    expect(await balanceOf(user, "NGN")).toBe(0);
    await expectBalanced("NGN_PLAY");
    await expectBalanced("NGN");
  });

  it("a ledger instance refuses any currency but its own, even if called directly", async () => {
    await fund(uid("u"), 100, "NGN_PLAY"); // pins the play ledger to NGN_PLAY
    const playLedger = ledger("NGN_PLAY");

    const result = await playLedger.deposit({
      opId: uid("dep"),
      currency: "NGN",
      userId: uid("u"),
      amount: 100,
      provider: "paystack",
    });
    expectFailure(result, "CURRENCY_MISMATCH");
  });
});
