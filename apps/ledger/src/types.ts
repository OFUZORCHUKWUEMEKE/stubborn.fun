import type { Currency } from "@stubborn/money";

/**
 * Typed failures. These cross the RPC boundary as data rather than thrown
 * errors: custom error classes do not survive serialisation between
 * Workers, so a caller could not otherwise tell "insufficient funds" from
 * an outage. Infrastructure failures still throw.
 */
export type LedgerErrorCode =
  | "INVALID_INPUT"
  | "INSUFFICIENT_FUNDS"
  | "IDEMPOTENCY_CONFLICT"
  | "CURRENCY_MISMATCH"
  | "ESCROW_NOT_FOUND"
  | "ESCROW_CLOSED"
  | "ALREADY_STAKED"
  | "NOT_A_PARTICIPANT"
  | "WITHDRAWAL_EXISTS"
  | "WITHDRAWAL_NOT_FOUND"
  | "WITHDRAWAL_CLOSED";

export interface LedgerFailure {
  code: LedgerErrorCode;
  message: string;
}

/**
 * The outcome of a money operation. `replayed` is true when the op id had
 * already been applied: the stored result of the original is returned and
 * nothing moves again.
 */
export type LedgerResult<T> =
  | { ok: true; value: T; replayed: boolean }
  | { ok: false; error: LedgerFailure };

/**
 * Every money operation carries a caller-chosen op id. Repeating an op id
 * with the same request is a safe no-op; repeating it with a different
 * request is an IDEMPOTENCY_CONFLICT. Failed operations are not recorded,
 * so an op that failed (e.g. for insufficient funds) can be retried under
 * the same id once the cause is fixed.
 */
interface Op {
  opId: string;
  currency: Currency;
}

/** Real money arriving from a payment provider, or play money being issued. */
export interface DepositInput extends Op {
  userId: string;
  amount: number;
  provider: string;
}
export interface DepositResult {
  userBalance: number;
}

export interface LockStakeInput extends Op {
  gameId: string;
  userId: string;
  amount: number;
}
export interface LockStakeResult {
  userBalance: number;
  escrowBalance: number;
}

export interface SettleGameInput extends Op {
  gameId: string;
  winnerUserId: string;
}
export interface SettleGameResult {
  pot: number;
  payout: number;
  fee: number;
  feeBps: number;
  winnerUserId: string;
}

export interface RefundGameInput extends Op {
  gameId: string;
}
export interface RefundGameResult {
  refunds: { userId: string; amount: number }[];
}

export interface HoldWithdrawalInput extends Op {
  withdrawalId: string;
  userId: string;
  amount: number;
  provider: string;
}
export interface HoldWithdrawalResult {
  userBalance: number;
}

export interface CloseWithdrawalInput extends Op {
  withdrawalId: string;
}
export interface CloseWithdrawalResult {
  withdrawalId: string;
  status: "completed" | "reversed";
}

export interface EscrowView {
  gameId: string;
  currency: Currency;
  status: "open" | "settled" | "refunded";
  balance: number;
  feeBps: number;
  stakes: { userId: string; amount: number }[];
}

export interface ReconcileReport {
  balanced: boolean;
  issues: string[];
  checked: {
    accounts: number;
    entries: number;
    ops: number;
    escrows: number;
    withdrawals: number;
  };
}
