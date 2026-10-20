import { evictDurableObject, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { balanceOf, expectBalanced, expectOk, fund, ledger, service, uid } from "./helpers";

/** Runs raw SQL inside the ledger, bypassing every operation's checks. */
function raw<T>(fn: (sql: SqlStorage) => T): Promise<T> {
  return runInDurableObject(ledger("NGN"), (_instance, state) => fn(state.storage.sql));
}

describe("the schema itself refuses bad writes", () => {
  it("entries cannot be updated or deleted", async () => {
    await fund(uid("u"), 100);

    await expect(raw((sql) => sql.exec("UPDATE entries SET amount = amount + 1"))).rejects.toThrow(/append-only/);
    await expect(raw((sql) => sql.exec("DELETE FROM entries"))).rejects.toThrow(/append-only/);
    await expectBalanced();
  });

  it("ops cannot be updated or deleted", async () => {
    await fund(uid("u"), 100);

    await expect(raw((sql) => sql.exec("UPDATE ops SET result = '{}'"))).rejects.toThrow(/append-only/);
    await expect(raw((sql) => sql.exec("DELETE FROM ops"))).rejects.toThrow(/append-only/);
  });

  it("an entry that would overdraw a user is refused even when the code's check is bypassed", async () => {
    const user = uid("u");
    await fund(user, 500);

    await expect(
      raw((sql) => {
        const { id } = sql.exec<{ id: number }>("SELECT id FROM accounts WHERE ref = ?", `user:${user}`).one();
        sql.exec(
          "INSERT INTO entries (op_id, account_id, amount, created_at) VALUES ('rogue', ?, -501, 0)",
          id,
        );
      }),
    ).rejects.toThrow(/CHECK constraint failed/);

    expect(await balanceOf(user)).toBe(500);
    await expectBalanced();
  });

  it("zero-amount entries are refused", async () => {
    const user = uid("u");
    await fund(user, 500);
    await expect(
      raw((sql) => {
        const { id } = sql.exec<{ id: number }>("SELECT id FROM accounts WHERE ref = ?", `user:${user}`).one();
        sql.exec("INSERT INTO entries (op_id, account_id, amount, created_at) VALUES ('rogue', ?, 0, 0)", id);
      }),
    ).rejects.toThrow(/CHECK constraint failed/);
  });

  it("balances follow entries through the trigger, never through direct writes", async () => {
    const user = uid("u");
    await fund(user, 1_000);
    const { balance, posted } = await raw((sql) =>
      sql
        .exec<{ balance: number; posted: number }>(
          `SELECT a.balance, (SELECT SUM(amount) FROM entries WHERE account_id = a.id) AS posted
             FROM accounts a WHERE a.ref = ?`,
          `user:${user}`,
        )
        .one(),
    );
    expect(balance).toBe(1_000);
    expect(posted).toBe(1_000);
  });
});

describe("reconcile", () => {
  it("detects a balance tampered with outside the ledger's operations", async () => {
    const user = uid("u");
    await fund(user, 1_000);
    await expectBalanced();

    await raw((sql) => sql.exec("UPDATE accounts SET balance = balance + 1 WHERE ref = ?", `user:${user}`));
    try {
      const report = await service.reconcile("NGN");
      expect(report.balanced).toBe(false);
      expect(report.issues.some((i) => i.includes(`user:${user}`) && i.includes("sum of entries"))).toBe(true);
      expect(report.issues.some((i) => i.includes("sum to 1, not 0"))).toBe(true);
    } finally {
      // Undo, so the shared ledger is consistent for later tests.
      await raw((sql) => sql.exec("UPDATE accounts SET balance = balance - 1 WHERE ref = ?", `user:${user}`));
    }
    await expectBalanced();
  });

  it("detects an escrow that no longer matches its stakes", async () => {
    const user = uid("u");
    await fund(user, 1_000);
    const gameId = uid("game");
    expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: user, amount: 1_000 }));

    await raw((sql) => sql.exec("UPDATE stakes SET amount = 999 WHERE game_id = ?", gameId));
    try {
      const report = await service.reconcile("NGN");
      expect(report.issues.some((i) => i.includes(`escrow ${gameId}`))).toBe(true);
    } finally {
      await raw((sql) => sql.exec("UPDATE stakes SET amount = 1000 WHERE game_id = ?", gameId));
    }
    await expectBalanced();
  });
});

describe("durability", () => {
  it("survives eviction: all state is in SQLite, none in memory", async () => {
    const [a, b] = [uid("a"), uid("b")];
    await fund(a, 3_000);
    await fund(b, 3_000);
    const gameId = uid("game");
    expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: a, amount: 3_000 }));

    await evictDurableObject(ledger("NGN"));

    // A fresh instance reads everything back and keeps working.
    expect(await balanceOf(a)).toBe(0);
    expectOk(await service.lockStake({ opId: uid("stk"), currency: "NGN", gameId, userId: b, amount: 3_000 }));
    const settled = expectOk(await service.settleGame({ opId: uid("set"), currency: "NGN", gameId, winnerUserId: b }));
    expect(settled.pot).toBe(6_000);
    await expectBalanced();
  });

  it("an op replayed after eviction is still recognised", async () => {
    const user = uid("u");
    const req = { opId: uid("dep"), currency: "NGN" as const, userId: user, amount: 400, provider: "paystack" };
    expectOk(await service.deposit(req));

    await evictDurableObject(ledger("NGN"));

    const again = await service.deposit(req);
    expect(again.ok && again.replayed).toBe(true);
    expect(await balanceOf(user)).toBe(400);
  });
});
