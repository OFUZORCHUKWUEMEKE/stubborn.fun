import { WorkerEntrypoint } from "cloudflare:workers";
import { isCurrency, type Currency } from "@stubborn/money";
import type { LedgerDurableObject } from "./ledger-do";
import type {
  CloseWithdrawalInput,
  CloseWithdrawalResult,
  DepositInput,
  DepositResult,
  EscrowView,
  HoldWithdrawalInput,
  HoldWithdrawalResult,
  LedgerResult,
  LockStakeInput,
  LockStakeResult,
  ReconcileReport,
  RefundGameInput,
  RefundGameResult,
  SettleGameInput,
  SettleGameResult,
} from "./types";

export { LedgerDurableObject } from "./ledger-do";
export type * from "./types";

/**
 * The ledger's only entry point. Other Workers bind to this class by name
 * (a service binding with `entrypoint: "LedgerService"`) and can call
 * nothing but these methods: there is no fetch route into the ledger and
 * no way to run arbitrary queries against it.
 *
 * Each currency has its own Durable Object, so real money and play money
 * live in separate databases. Every call names its currency, and the
 * target instance refuses any currency other than the one it holds.
 */
export class LedgerService extends WorkerEntrypoint<Env> {
  deposit(input: DepositInput): Promise<LedgerResult<DepositResult>> {
    return this.route(input.currency, (l) => l.deposit(input));
  }

  lockStake(input: LockStakeInput): Promise<LedgerResult<LockStakeResult>> {
    return this.route(input.currency, (l) => l.lockStake(input));
  }

  settleGame(input: SettleGameInput): Promise<LedgerResult<SettleGameResult>> {
    return this.route(input.currency, (l) => l.settleGame(input));
  }

  refundGame(input: RefundGameInput): Promise<LedgerResult<RefundGameResult>> {
    return this.route(input.currency, (l) => l.refundGame(input));
  }

  holdWithdrawal(input: HoldWithdrawalInput): Promise<LedgerResult<HoldWithdrawalResult>> {
    return this.route(input.currency, (l) => l.holdWithdrawal(input));
  }

  completeWithdrawal(input: CloseWithdrawalInput): Promise<LedgerResult<CloseWithdrawalResult>> {
    return this.route(input.currency, (l) => l.completeWithdrawal(input));
  }

  reverseWithdrawal(input: CloseWithdrawalInput): Promise<LedgerResult<CloseWithdrawalResult>> {
    return this.route(input.currency, (l) => l.reverseWithdrawal(input));
  }

  balance(input: { currency: Currency; userId: string }): Promise<LedgerResult<{ balance: number }>> {
    return this.route(input.currency, (l) => l.balance(input));
  }

  escrow(input: { currency: Currency; gameId: string }): Promise<LedgerResult<EscrowView>> {
    return this.route(input.currency, (l) => l.escrow(input));
  }

  async reconcile(currency: Currency): Promise<ReconcileReport> {
    if (!isCurrency(currency)) throw new Error(`unknown currency: ${String(currency)}`);
    return detach(await ledgerFor(this.env, currency).reconcile());
  }

  private async route<T>(
    currency: unknown,
    call: (ledger: DurableObjectStub<LedgerDurableObject>) => Promise<LedgerResult<T>>,
  ): Promise<LedgerResult<T>> {
    if (!isCurrency(currency)) {
      return { ok: false, error: { code: "INVALID_INPUT", message: `unknown currency: ${String(currency)}` } };
    }
    return detach(await call(ledgerFor(this.env, currency)));
  }
}

/**
 * Copies a Durable Object's RPC reply into plain data and releases the
 * original. The reply holds a runtime reference until it is disposed;
 * passing it straight back to our own caller would leave that release to
 * the garbage collector, which the runtime flags as a leak. Ledger results
 * are plain data, so the copy is exact.
 */
function detach<T>(result: T): T {
  try {
    return structuredClone(result);
  } finally {
    (result as Partial<Disposable>)[Symbol.dispose]?.();
  }
}

/**
 * The ledger instance for a currency. The location hint asks Cloudflare to
 * place it in Africa, near the players; it is honoured only when the
 * object is first created, and only on a best-effort basis.
 */
export function ledgerFor(env: Env, currency: Currency): DurableObjectStub<LedgerDurableObject> {
  return env.LEDGER.get(env.LEDGER.idFromName(`ledger:${currency}`), { locationHint: "afr" });
}

export default {
  fetch(): Response {
    return new Response("Not found", { status: 404 });
  },
} satisfies ExportedHandler<Env>;
