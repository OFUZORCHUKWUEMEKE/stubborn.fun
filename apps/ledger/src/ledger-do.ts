import { DurableObject } from "cloudflare:workers";
import {
  MoneyError,
  bps,
  isCurrency,
  kobo,
  positiveKobo,
  splitPot,
  type Bps,
  type Currency,
  type Kobo,
} from "@stubborn/money";
import { SCHEMA_STATEMENTS, SCHEMA_VERSION } from "./schema";
import type {
  CloseWithdrawalInput,
  CloseWithdrawalResult,
  DepositInput,
  DepositResult,
  EscrowView,
  HoldWithdrawalInput,
  HoldWithdrawalResult,
  LedgerErrorCode,
  LedgerResult,
  LockStakeInput,
  LockStakeResult,
  ReconcileReport,
  RefundGameInput,
  RefundGameResult,
  SettleGameInput,
  SettleGameResult,
} from "./types";

/** A business-rule refusal. Thrown inside a transaction to roll it back. */
class Rejection extends Error {
  constructor(
    readonly code: LedgerErrorCode,
    message: string,
  ) {
    super(message);
  }
}

/** Ids of users, games and withdrawals. No ':' so account refs stay unambiguous. */
const ENTITY_ID = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/;
/** Caller-chosen op ids, e.g. "stake:<gameId>:<userId>". */
const OP_ID = /^[A-Za-z0-9][A-Za-z0-9:_.-]{0,255}$/;
/** Payment provider names, e.g. "paystack". */
const PROVIDER = /^[a-z][a-z0-9_]{1,31}$/;

const HOUSE_FEES = "house:fees";
const PENDING_WITHDRAWALS = "pending:withdrawals";
const userRef = (userId: string) => `user:${userId}`;
const escrowRef = (gameId: string) => `escrow:${gameId}`;
const clearingRef = (provider: string) => `clearing:${provider}`;

/**
 * Clearing accounts mirror money held at a payment provider, so they run
 * negative as users deposit. Every other account must stay at or above
 * zero. The rule is derived from the ref, never passed in, so a caller
 * cannot open a negative-capable user account.
 */
const allowsNegative = (ref: string) => ref.startsWith("clearing:");

type Row = Record<string, SqlStorageValue>;

/**
 * The single source of truth for money in one currency. Each currency gets
 * its own instance (see LedgerService), so play money and real money can
 * never mix and play-money load cannot slow real-money operations.
 *
 * A Durable Object handles one event at a time, and every operation here
 * runs as one synchronous SQLite transaction with no await inside it, so
 * operations are fully serialised: no two can interleave, and there are no
 * check-then-act races to defend against. The operation methods are
 * deliberately not async, which makes an await inside one a compile error
 * rather than a convention.
 */
export class LedgerDurableObject extends DurableObject<Env> {
  private readonly sql: SqlStorage;
  private readonly feeBps: Bps;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    // Fail loudly at startup on a misconfigured fee rather than on the
    // first settlement.
    this.feeBps = bps(Number(env.GAME_FEE_BPS));
    ctx.storage.transactionSync(() => {
      for (const statement of SCHEMA_STATEMENTS) this.sql.exec(statement);
      this.sql.exec(
        "INSERT INTO meta (key, value) VALUES ('schema_version', ?) ON CONFLICT (key) DO NOTHING",
        String(SCHEMA_VERSION),
      );
    });
  }

  // ---------------------------------------------------------------------
  // Money operations. Each is all-or-nothing and idempotent on its op id.
  // ---------------------------------------------------------------------

  /** Credits a user from a provider's clearing account. */
  deposit(input: DepositInput): LedgerResult<DepositResult> {
    return this.guard(() => {
      const req = {
        currency: this.currency(input.currency),
        userId: this.entityId("userId", input.userId),
        amount: positiveKobo(input.amount),
        provider: this.provider(input.provider),
      };
      return this.run("deposit", input.opId, req, (opId, now) => {
        const clearing = this.account(clearingRef(req.provider), req.currency, now);
        const user = this.account(userRef(req.userId), req.currency, now);
        this.post(opId, now, [
          [clearing.id, kobo(-req.amount)],
          [user.id, req.amount],
        ]);
        return { userBalance: this.balanceOf(user.id) };
      });
    });
  }

  /**
   * Moves a player's stake into the game's escrow. The first stake opens
   * the escrow and fixes its fee rate for the life of the game.
   */
  lockStake(input: LockStakeInput): LedgerResult<LockStakeResult> {
    return this.guard(() => {
      const req = {
        currency: this.currency(input.currency),
        gameId: this.entityId("gameId", input.gameId),
        userId: this.entityId("userId", input.userId),
        amount: positiveKobo(input.amount),
      };
      return this.run("lock_stake", input.opId, req, (opId, now) => {
        const escrow = this.openOrCreateEscrow(req.gameId, req.currency, now);
        const existing = this.sql
          .exec("SELECT 1 FROM stakes WHERE game_id = ? AND user_id = ?", req.gameId, req.userId)
          .toArray();
        if (existing.length > 0) {
          throw new Rejection("ALREADY_STAKED", `${req.userId} already has a stake in ${req.gameId}`);
        }

        const user = this.account(userRef(req.userId), req.currency, now);
        this.requireFunds(user, req.amount);
        this.post(opId, now, [
          [user.id, kobo(-req.amount)],
          [escrow.accountId, req.amount],
        ]);
        this.sql.exec(
          "INSERT INTO stakes (game_id, user_id, amount, op_id, created_at) VALUES (?, ?, ?, ?, ?)",
          req.gameId,
          req.userId,
          req.amount,
          opId,
          now,
        );
        return {
          userBalance: this.balanceOf(user.id),
          escrowBalance: this.balanceOf(escrow.accountId),
        };
      });
    });
  }

  /**
   * Pays the whole pot to the winner less the platform fee. The pot is the
   * escrow balance the ledger itself holds, never a figure from the caller,
   * and payout + fee === pot exactly.
   */
  settleGame(input: SettleGameInput): LedgerResult<SettleGameResult> {
    return this.guard(() => {
      const req = {
        currency: this.currency(input.currency),
        gameId: this.entityId("gameId", input.gameId),
        winnerUserId: this.entityId("winnerUserId", input.winnerUserId),
      };
      return this.run("settle_game", input.opId, req, (opId, now) => {
        const escrow = this.openEscrow(req.gameId, req.currency);
        const staked = this.sql
          .exec("SELECT 1 FROM stakes WHERE game_id = ? AND user_id = ?", req.gameId, req.winnerUserId)
          .toArray();
        if (staked.length === 0) {
          throw new Rejection("NOT_A_PARTICIPANT", `${req.winnerUserId} has no stake in ${req.gameId}`);
        }

        const pot = kobo(this.balanceOf(escrow.accountId));
        this.requireEscrowMatchesStakes(req.gameId, pot);
        const { payout, fee } = splitPot(pot, bps(escrow.feeBps));

        const winner = this.account(userRef(req.winnerUserId), req.currency, now);
        const house = this.account(HOUSE_FEES, req.currency, now);
        this.post(opId, now, [
          [escrow.accountId, kobo(-pot)],
          [winner.id, payout],
          [house.id, fee],
        ]);
        this.closeEscrow(req.gameId, escrow.accountId, "settled", opId);

        return { pot, payout, fee, feeBps: escrow.feeBps, winnerUserId: req.winnerUserId };
      });
    });
  }

  /** Returns every stake to its owner: draws, abandoned and expired games. */
  refundGame(input: RefundGameInput): LedgerResult<RefundGameResult> {
    return this.guard(() => {
      const req = {
        currency: this.currency(input.currency),
        gameId: this.entityId("gameId", input.gameId),
      };
      return this.run("refund_game", input.opId, req, (opId, now) => {
        const escrow = this.openEscrow(req.gameId, req.currency);
        const stakes = this.sql
          .exec<{ user_id: string; amount: number }>(
            "SELECT user_id, amount FROM stakes WHERE game_id = ? ORDER BY user_id",
            req.gameId,
          )
          .toArray();

        const pot = kobo(this.balanceOf(escrow.accountId));
        this.requireEscrowMatchesStakes(req.gameId, pot);

        const legs: [number, Kobo][] = [[escrow.accountId, kobo(-pot)]];
        for (const s of stakes) {
          legs.push([this.account(userRef(s.user_id), req.currency, now).id, kobo(s.amount)]);
        }
        this.post(opId, now, legs);
        this.closeEscrow(req.gameId, escrow.accountId, "refunded", opId);

        return { refunds: stakes.map((s) => ({ userId: s.user_id, amount: s.amount })) };
      });
    });
  }

  /** Reserves funds for a withdrawal until the provider confirms or fails it. */
  holdWithdrawal(input: HoldWithdrawalInput): LedgerResult<HoldWithdrawalResult> {
    return this.guard(() => {
      const req = {
        currency: this.currency(input.currency),
        withdrawalId: this.entityId("withdrawalId", input.withdrawalId),
        userId: this.entityId("userId", input.userId),
        amount: positiveKobo(input.amount),
        provider: this.provider(input.provider),
      };
      return this.run("hold_withdrawal", input.opId, req, (opId, now) => {
        const exists = this.sql
          .exec("SELECT 1 FROM withdrawals WHERE withdrawal_id = ?", req.withdrawalId)
          .toArray();
        if (exists.length > 0) {
          throw new Rejection("WITHDRAWAL_EXISTS", `withdrawal ${req.withdrawalId} already exists`);
        }

        const user = this.account(userRef(req.userId), req.currency, now);
        this.requireFunds(user, req.amount);
        const pending = this.account(PENDING_WITHDRAWALS, req.currency, now);
        this.post(opId, now, [
          [user.id, kobo(-req.amount)],
          [pending.id, req.amount],
        ]);
        this.sql.exec(
          `INSERT INTO withdrawals (withdrawal_id, user_id, currency, amount, provider, status, created_at)
           VALUES (?, ?, ?, ?, ?, 'held', ?)`,
          req.withdrawalId,
          req.userId,
          req.currency,
          req.amount,
          req.provider,
          now,
        );
        return { userBalance: this.balanceOf(user.id) };
      });
    });
  }

  /** The provider paid out: the held funds leave the platform. */
  completeWithdrawal(input: CloseWithdrawalInput): LedgerResult<CloseWithdrawalResult> {
    return this.closeWithdrawal("complete_withdrawal", input, "completed");
  }

  /** The provider failed: the held funds go back to the user. */
  reverseWithdrawal(input: CloseWithdrawalInput): LedgerResult<CloseWithdrawalResult> {
    return this.closeWithdrawal("reverse_withdrawal", input, "reversed");
  }

  // ---------------------------------------------------------------------
  // Reads
  // ---------------------------------------------------------------------

  balance(input: { currency: Currency; userId: string }): LedgerResult<{ balance: number }> {
    return this.guard(() => {
      const currency = this.currency(input.currency);
      const userId = this.entityId("userId", input.userId);
      const row = this.sql
        .exec<{ balance: number }>(
          "SELECT balance FROM accounts WHERE ref = ? AND currency = ?",
          userRef(userId),
          currency,
        )
        .toArray()[0];
      return { ok: true, value: { balance: row?.balance ?? 0 }, replayed: false };
    });
  }

  escrow(input: { currency: Currency; gameId: string }): LedgerResult<EscrowView> {
    return this.guard(() => {
      const currency = this.currency(input.currency);
      const gameId = this.entityId("gameId", input.gameId);
      const row = this.sql
        .exec<{ currency: string; status: string; fee_bps: number; balance: number }>(
          `SELECT x.currency, x.status, x.fee_bps, a.balance
             FROM escrows x JOIN accounts a ON a.id = x.account_id
            WHERE x.game_id = ?`,
          gameId,
        )
        .toArray()[0];
      if (!row) throw new Rejection("ESCROW_NOT_FOUND", `no escrow for ${gameId}`);
      if (row.currency !== currency) {
        throw new Rejection("CURRENCY_MISMATCH", `escrow ${gameId} is ${row.currency}, not ${currency}`);
      }
      const stakes = this.sql
        .exec<{ user_id: string; amount: number }>(
          "SELECT user_id, amount FROM stakes WHERE game_id = ? ORDER BY user_id",
          gameId,
        )
        .toArray();
      return {
        ok: true,
        replayed: false,
        value: {
          gameId,
          currency,
          status: row.status as EscrowView["status"],
          balance: row.balance,
          feeBps: row.fee_bps,
          stakes: stakes.map((s) => ({ userId: s.user_id, amount: s.amount })),
        },
      };
    });
  }

  /**
   * Re-derives every invariant from the raw tables. Under normal operation
   * this always reports balanced; any issue means something wrote outside
   * the operations above, or a bug got past the schema's guards. Meant for
   * a scheduled job, not a request path.
   */
  reconcile(): ReconcileReport {
    const issues: string[] = [];

    for (const r of this.sql.exec<{ currency: string; total: number }>(
      "SELECT currency, SUM(balance) AS total FROM accounts GROUP BY currency HAVING total <> 0",
    )) {
      issues.push(`balances in ${r.currency} sum to ${r.total}, not 0`);
    }

    for (const r of this.sql.exec<{ ref: string; balance: number; posted: number }>(
      `SELECT a.ref, a.balance, COALESCE(SUM(e.amount), 0) AS posted
         FROM accounts a LEFT JOIN entries e ON e.account_id = a.id
        GROUP BY a.id HAVING a.balance <> posted`,
    )) {
      issues.push(`account ${r.ref} balance ${r.balance} != sum of entries ${r.posted}`);
    }

    for (const r of this.sql.exec<{ op_id: string; total: number }>(
      "SELECT op_id, SUM(amount) AS total FROM entries GROUP BY op_id HAVING total <> 0",
    )) {
      issues.push(`op ${r.op_id} entries sum to ${r.total}, not 0`);
    }

    for (const r of this.sql.exec<{ op_id: string }>(
      `SELECT DISTINCT e.op_id FROM entries e LEFT JOIN ops o ON o.op_id = e.op_id
        WHERE o.op_id IS NULL`,
    )) {
      issues.push(`entries reference unrecorded op ${r.op_id}`);
    }

    for (const r of this.sql.exec<{ game_id: string; status: string; balance: number; staked: number }>(
      `SELECT x.game_id, x.status, a.balance,
              COALESCE((SELECT SUM(amount) FROM stakes s WHERE s.game_id = x.game_id), 0) AS staked
         FROM escrows x JOIN accounts a ON a.id = x.account_id`,
    )) {
      const expected = r.status === "open" ? r.staked : 0;
      if (r.balance !== expected) {
        issues.push(`escrow ${r.game_id} (${r.status}) holds ${r.balance}, expected ${expected}`);
      }
    }

    for (const r of this.sql.exec<{ currency: string; balance: number; held: number }>(
      `SELECT a.currency, a.balance,
              COALESCE((SELECT SUM(amount) FROM withdrawals w
                         WHERE w.status = 'held' AND w.currency = a.currency), 0) AS held
         FROM accounts a WHERE a.ref = ?`,
      PENDING_WITHDRAWALS,
    )) {
      if (r.balance !== r.held) {
        issues.push(`pending withdrawals in ${r.currency} hold ${r.balance}, but held withdrawals total ${r.held}`);
      }
    }

    const count = (table: string) =>
      this.sql.exec<{ n: number }>(`SELECT COUNT(*) AS n FROM ${table}`).one().n;

    return {
      balanced: issues.length === 0,
      issues,
      checked: {
        accounts: count("accounts"),
        entries: count("entries"),
        ops: count("ops"),
        escrows: count("escrows"),
        withdrawals: count("withdrawals"),
      },
    };
  }

  // ---------------------------------------------------------------------
  // Internals
  // ---------------------------------------------------------------------

  /** Converts input-validation throws into typed failures. */
  private guard<T>(fn: () => LedgerResult<T>): LedgerResult<T> {
    try {
      return fn();
    } catch (e) {
      if (e instanceof Rejection) return { ok: false, error: { code: e.code, message: e.message } };
      if (e instanceof MoneyError) return { ok: false, error: { code: "INVALID_INPUT", message: e.message } };
      throw e;
    }
  }

  /**
   * Runs one operation as one transaction, idempotently. Order inside the
   * transaction: pin this instance's currency, replay-or-conflict on the op
   * id, apply the body, assert the op's entries sum to zero, then record
   * the op. Any throw rolls back everything, including the op record, so a
   * failed op leaves no trace and can be retried under the same id.
   */
  private run<T>(kind: string, opIdInput: string, request: object, body: (opId: string, now: number) => T): LedgerResult<T> {
    const opId = this.opId(opIdInput);
    const canonical = stableStringify(request);
    const now = Date.now();

    return this.ctx.storage.transactionSync(() => {
      this.pinCurrency((request as { currency: Currency }).currency);

      const prior = this.sql
        .exec<{ kind: string; request: string; result: string }>(
          "SELECT kind, request, result FROM ops WHERE op_id = ?",
          opId,
        )
        .toArray()[0];
      if (prior) {
        if (prior.kind !== kind || prior.request !== canonical) {
          throw new Rejection(
            "IDEMPOTENCY_CONFLICT",
            `op ${opId} was already used for a different ${prior.kind} request`,
          );
        }
        return { ok: true, value: JSON.parse(prior.result) as T, replayed: true };
      }

      const value = body(opId, now);

      const net = this.sql
        .exec<{ total: number | null }>("SELECT SUM(amount) AS total FROM entries WHERE op_id = ?", opId)
        .one().total;
      if (net !== null && net !== 0) {
        // Unreachable unless a body posts unbalanced legs. Throwing rolls
        // the whole op back rather than committing money from nowhere.
        throw new Error(`ledger invariant violated: op ${opId} entries sum to ${net}`);
      }

      this.sql.exec(
        "INSERT INTO ops (op_id, kind, request, result, created_at) VALUES (?, ?, ?, ?, ?)",
        opId,
        kind,
        canonical,
        JSON.stringify(value),
        now,
      );
      return { ok: true, value, replayed: false };
    });
  }

  /**
   * Binds this instance to one currency on its first operation and refuses
   * any other afterwards, so a routing mistake cannot mix currencies.
   */
  private pinCurrency(currency: Currency): void {
    this.sql.exec("INSERT INTO meta (key, value) VALUES ('currency', ?) ON CONFLICT (key) DO NOTHING", currency);
    const pinned = this.sql.exec<{ value: string }>("SELECT value FROM meta WHERE key = 'currency'").one().value;
    if (pinned !== currency) {
      throw new Rejection("CURRENCY_MISMATCH", `this ledger holds ${pinned}, not ${currency}`);
    }
  }

  private account(ref: string, currency: Currency, now: number): { id: number; balance: number; allowNegative: boolean } {
    this.sql.exec(
      `INSERT INTO accounts (ref, currency, balance, allow_negative, created_at)
       VALUES (?, ?, 0, ?, ?) ON CONFLICT (ref, currency) DO NOTHING`,
      ref,
      currency,
      allowsNegative(ref) ? 1 : 0,
      now,
    );
    const row = this.sql
      .exec<{ id: number; balance: number; allow_negative: number }>(
        "SELECT id, balance, allow_negative FROM accounts WHERE ref = ? AND currency = ?",
        ref,
        currency,
      )
      .one();
    return { id: row.id, balance: row.balance, allowNegative: row.allow_negative === 1 };
  }

  private balanceOf(accountId: number): number {
    return this.sql.exec<{ balance: number }>("SELECT balance FROM accounts WHERE id = ?", accountId).one().balance;
  }

  /** The friendly funds check. The schema's CHECK constraint is the backstop. */
  private requireFunds(account: { balance: number; allowNegative: boolean }, amount: Kobo): void {
    if (!account.allowNegative && account.balance < amount) {
      throw new Rejection("INSUFFICIENT_FUNDS", `balance ${account.balance} is less than ${amount}`);
    }
  }

  /**
   * Appends one entry per leg. Balances follow via the schema trigger, in
   * the same statement, so they cannot be forgotten. Zero legs are skipped
   * (a 0% fee posts nothing to the house) because entries must be non-zero.
   */
  private post(opId: string, now: number, legs: readonly (readonly [number, Kobo])[]): void {
    for (const [accountId, amount] of legs) {
      if (amount === 0) continue;
      this.sql.exec(
        "INSERT INTO entries (op_id, account_id, amount, created_at) VALUES (?, ?, ?, ?)",
        opId,
        accountId,
        amount,
        now,
      );
    }
  }

  private openOrCreateEscrow(gameId: string, currency: Currency, now: number): { accountId: number; feeBps: number } {
    const existing = this.sql
      .exec<{ currency: string; status: string; account_id: number; fee_bps: number }>(
        "SELECT currency, status, account_id, fee_bps FROM escrows WHERE game_id = ?",
        gameId,
      )
      .toArray()[0];
    if (existing) {
      if (existing.currency !== currency) {
        throw new Rejection("CURRENCY_MISMATCH", `game ${gameId} is ${existing.currency}, not ${currency}`);
      }
      if (existing.status !== "open") {
        throw new Rejection("ESCROW_CLOSED", `game ${gameId} is already ${existing.status}`);
      }
      return { accountId: existing.account_id, feeBps: existing.fee_bps };
    }

    const account = this.account(escrowRef(gameId), currency, now);
    this.sql.exec(
      `INSERT INTO escrows (game_id, currency, account_id, fee_bps, status, created_at)
       VALUES (?, ?, ?, ?, 'open', ?)`,
      gameId,
      currency,
      account.id,
      this.feeBps,
      now,
    );
    return { accountId: account.id, feeBps: this.feeBps };
  }

  private openEscrow(gameId: string, currency: Currency): { accountId: number; feeBps: number } {
    const row = this.sql
      .exec<{ currency: string; status: string; account_id: number; fee_bps: number }>(
        "SELECT currency, status, account_id, fee_bps FROM escrows WHERE game_id = ?",
        gameId,
      )
      .toArray()[0];
    if (!row) throw new Rejection("ESCROW_NOT_FOUND", `no escrow for ${gameId}`);
    if (row.currency !== currency) {
      throw new Rejection("CURRENCY_MISMATCH", `game ${gameId} is ${row.currency}, not ${currency}`);
    }
    if (row.status !== "open") {
      throw new Rejection("ESCROW_CLOSED", `game ${gameId} is already ${row.status}`);
    }
    return { accountId: row.account_id, feeBps: row.fee_bps };
  }

  /** Settling or refunding must pay out exactly what was staked. */
  private requireEscrowMatchesStakes(gameId: string, pot: Kobo): void {
    const staked = this.sql
      .exec<{ total: number | null }>("SELECT SUM(amount) AS total FROM stakes WHERE game_id = ?", gameId)
      .one().total ?? 0;
    if (staked !== pot) {
      throw new Error(`ledger invariant violated: escrow ${gameId} holds ${pot} but stakes total ${staked}`);
    }
  }

  private closeEscrow(gameId: string, accountId: number, status: "settled" | "refunded", opId: string): void {
    const remaining = this.balanceOf(accountId);
    if (remaining !== 0) {
      throw new Error(`ledger invariant violated: escrow ${gameId} left holding ${remaining}`);
    }
    this.sql.exec(
      "UPDATE escrows SET status = ?, closed_by_op = ? WHERE game_id = ? AND status = 'open'",
      status,
      opId,
      gameId,
    );
  }

  private closeWithdrawal(
    kind: string,
    input: CloseWithdrawalInput,
    outcome: "completed" | "reversed",
  ): LedgerResult<CloseWithdrawalResult> {
    return this.guard(() => {
      const req = {
        currency: this.currency(input.currency),
        withdrawalId: this.entityId("withdrawalId", input.withdrawalId),
      };
      return this.run(kind, input.opId, req, (opId, now) => {
        const w = this.sql
          .exec<{ user_id: string; currency: string; amount: number; provider: string; status: string }>(
            "SELECT user_id, currency, amount, provider, status FROM withdrawals WHERE withdrawal_id = ?",
            req.withdrawalId,
          )
          .toArray()[0];
        if (!w) throw new Rejection("WITHDRAWAL_NOT_FOUND", `no withdrawal ${req.withdrawalId}`);
        if (w.currency !== req.currency) {
          throw new Rejection("CURRENCY_MISMATCH", `withdrawal ${req.withdrawalId} is ${w.currency}`);
        }
        if (w.status !== "held") {
          throw new Rejection("WITHDRAWAL_CLOSED", `withdrawal ${req.withdrawalId} is already ${w.status}`);
        }

        const pending = this.account(PENDING_WITHDRAWALS, req.currency, now);
        const destination =
          outcome === "completed"
            ? this.account(clearingRef(w.provider), req.currency, now)
            : this.account(userRef(w.user_id), req.currency, now);
        this.post(opId, now, [
          [pending.id, kobo(-w.amount)],
          [destination.id, kobo(w.amount)],
        ]);
        this.sql.exec(
          "UPDATE withdrawals SET status = ?, closed_by_op = ? WHERE withdrawal_id = ? AND status = 'held'",
          outcome,
          opId,
          req.withdrawalId,
        );
        return { withdrawalId: req.withdrawalId, status: outcome };
      });
    });
  }

  // --- input validation (throws Rejection("INVALID_INPUT")) ---

  private currency(value: unknown): Currency {
    if (!isCurrency(value)) throw new Rejection("INVALID_INPUT", `unknown currency: ${String(value)}`);
    return value;
  }

  private entityId(field: string, value: unknown): string {
    if (typeof value !== "string" || !ENTITY_ID.test(value)) {
      throw new Rejection("INVALID_INPUT", `invalid ${field}: ${String(value)}`);
    }
    return value;
  }

  private opId(value: unknown): string {
    if (typeof value !== "string" || !OP_ID.test(value)) {
      throw new Rejection("INVALID_INPUT", `invalid opId: ${String(value)}`);
    }
    return value;
  }

  private provider(value: unknown): string {
    if (typeof value !== "string" || !PROVIDER.test(value)) {
      throw new Rejection("INVALID_INPUT", `invalid provider: ${String(value)}`);
    }
    return value;
  }
}

/** JSON with sorted keys, so equal requests always compare equal. */
function stableStringify(value: unknown): string {
  if (value === null || typeof value !== "object") return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(stableStringify).join(",")}]`;
  const obj = value as Record<string, unknown>;
  return `{${Object.keys(obj)
    .sort()
    .map((k) => `${JSON.stringify(k)}:${stableStringify(obj[k])}`)
    .join(",")}}`;
}
