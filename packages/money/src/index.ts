/**
 * Money primitives. Every amount in the system is an integer count of
 * kobo (1 NGN = 100 kobo) carried in a JS number. Numbers are exact for
 * integers up to 2^53 - 1 (about ₦90 trillion), and every constructor and
 * arithmetic helper here refuses to leave that range, so precision can
 * never be lost silently. There are no floats anywhere near money.
 */

/** Currencies the ledger accepts. NGN_PLAY is play money for the beta. */
export const CURRENCIES = ["NGN", "NGN_PLAY"] as const;
export type Currency = (typeof CURRENCIES)[number];

export function isCurrency(value: unknown): value is Currency {
  return typeof value === "string" && (CURRENCIES as readonly string[]).includes(value);
}

/** Thrown when a value cannot be represented exactly as money. */
export class MoneyError extends Error {
  override readonly name = "MoneyError";
}

declare const koboBrand: unique symbol;
/** An exact integer number of kobo. May be negative (signed ledger entries). */
export type Kobo = number & { readonly [koboBrand]: "Kobo" };

/** Validates and brands an integer kobo amount. */
export function kobo(value: number): Kobo {
  if (!Number.isSafeInteger(value)) {
    throw new MoneyError(`not an exact integer kobo amount: ${value}`);
  }
  // Normalise -0 so amounts compare and serialise predictably.
  return (value === 0 ? 0 : value) as Kobo;
}

/** Validates a strictly positive kobo amount, as every transfer requires. */
export function positiveKobo(value: number): Kobo {
  const k = kobo(value);
  if (k <= 0) {
    throw new MoneyError(`amount must be positive: ${value}`);
  }
  return k;
}

export const ZERO_KOBO: Kobo = kobo(0);

export function addKobo(a: Kobo, b: Kobo): Kobo {
  return kobo(a + b);
}

export function subKobo(a: Kobo, b: Kobo): Kobo {
  return kobo(a - b);
}

export function sumKobo(values: readonly Kobo[]): Kobo {
  let total = ZERO_KOBO;
  for (const v of values) total = addKobo(total, v);
  return total;
}

/** 100% expressed in basis points. */
export const BPS_DENOMINATOR = 10_000;

declare const bpsBrand: unique symbol;
/** A rate in basis points, 0 to 10000 inclusive. */
export type Bps = number & { readonly [bpsBrand]: "Bps" };

export function bps(value: number): Bps {
  if (!Number.isInteger(value) || value < 0 || value > BPS_DENOMINATOR) {
    throw new MoneyError(`basis points must be an integer in [0, ${BPS_DENOMINATOR}]: ${value}`);
  }
  return value as Bps;
}

/**
 * The platform fee on an amount: floor(amount * rate / 10000).
 *
 * The multiply goes through BigInt because amount * rate can exceed 2^53
 * even when both inputs are exact; truncating to a number before dividing
 * would round the fee. Flooring means the fee never exceeds the stated
 * rate, and any fraction of a kobo stays with the player.
 */
export function feeOf(amount: Kobo, rate: Bps): Kobo {
  if (amount < 0) {
    throw new MoneyError(`cannot take a fee on a negative amount: ${amount}`);
  }
  const fee = (BigInt(amount) * BigInt(rate)) / BigInt(BPS_DENOMINATOR);
  return kobo(Number(fee));
}

export interface PotSplit {
  /** What the winner receives. */
  payout: Kobo;
  /** What the house keeps. */
  fee: Kobo;
}

/**
 * Splits a pot between the winner and the house. The payout is derived as
 * pot - fee rather than computed independently, so payout + fee === pot
 * exactly for every input: no kobo is ever created or lost to rounding.
 */
export function splitPot(pot: Kobo, rate: Bps): PotSplit {
  const fee = feeOf(pot, rate);
  return { payout: subKobo(pot, fee), fee };
}
