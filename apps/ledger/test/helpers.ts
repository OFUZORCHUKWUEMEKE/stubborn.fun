import { env, exports } from "cloudflare:workers";
import { expect } from "vitest";
import type { Currency } from "@stubborn/money";
import { ledgerFor } from "../src/index";
import type { LedgerErrorCode, LedgerResult } from "../src/types";

let counter = 0;
/** A unique id per call, so tests never collide on users, games or ops. */
export function uid(prefix: string): string {
  counter += 1;
  return `${prefix}-${Date.now().toString(36)}-${counter}`;
}

/** The ledger exactly as other Workers see it: the RPC entrypoint. */
export const service = exports.LedgerService;

/** The Durable Object behind a currency, for white-box tests. */
export function ledger(currency: Currency = "NGN") {
  return ledgerFor(env, currency);
}

export function expectOk<T>(result: LedgerResult<T>): T {
  if (!result.ok) {
    throw new Error(`expected ok, got ${result.error.code}: ${result.error.message}`);
  }
  return result.value;
}

export function expectFailure<T>(result: LedgerResult<T>, code: LedgerErrorCode): void {
  expect(result.ok, `expected ${code}, got ok`).toBe(false);
  if (!result.ok) expect(result.error.code).toBe(code);
}

/** Funds a user from the Paystack clearing account. */
export async function fund(userId: string, amount: number, currency: Currency = "NGN"): Promise<void> {
  expectOk(
    await service.deposit({
      opId: uid("dep"),
      currency,
      userId,
      amount,
      provider: currency === "NGN" ? "paystack" : "play",
    }),
  );
}

export async function balanceOf(userId: string, currency: Currency = "NGN"): Promise<number> {
  return expectOk(await service.balance({ currency, userId })).balance;
}

export async function expectBalanced(currency: Currency = "NGN"): Promise<void> {
  const report = await service.reconcile(currency);
  expect(report.issues).toEqual([]);
  expect(report.balanced).toBe(true);
}
