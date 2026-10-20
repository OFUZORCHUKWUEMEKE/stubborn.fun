import fc from "fast-check";
import { describe, expect, it } from "vitest";
import {
  BPS_DENOMINATOR,
  MoneyError,
  addKobo,
  bps,
  feeOf,
  isCurrency,
  kobo,
  positiveKobo,
  splitPot,
  subKobo,
  sumKobo,
} from "../src/index.js";

const MAX = Number.MAX_SAFE_INTEGER;

describe("kobo", () => {
  it.each([0, 1, -1, 100, MAX, -MAX])("accepts exact integer %d", (n) => {
    expect(kobo(n)).toBe(n === 0 ? 0 : n);
  });

  it.each([0.5, -0.01, Number.NaN, Infinity, -Infinity, MAX + 1, -(MAX + 1), 2 ** 60])(
    "rejects %d",
    (n) => {
      expect(() => kobo(n)).toThrow(MoneyError);
    },
  );

  it("normalises negative zero", () => {
    expect(Object.is(kobo(-0), 0)).toBe(true);
  });

  it("positiveKobo rejects zero and negatives", () => {
    expect(() => positiveKobo(0)).toThrow(MoneyError);
    expect(() => positiveKobo(-5)).toThrow(MoneyError);
    expect(positiveKobo(1)).toBe(1);
  });
});

describe("arithmetic never leaves the exact range", () => {
  it("addKobo throws instead of losing precision", () => {
    expect(() => addKobo(kobo(MAX), kobo(1))).toThrow(MoneyError);
  });

  it("subKobo throws instead of losing precision", () => {
    expect(() => subKobo(kobo(-MAX), kobo(1))).toThrow(MoneyError);
  });

  it("sumKobo totals exactly and throws on overflow", () => {
    expect(sumKobo([kobo(1), kobo(2), kobo(3)])).toBe(6);
    expect(sumKobo([])).toBe(0);
    expect(() => sumKobo([kobo(MAX), kobo(MAX)])).toThrow(MoneyError);
  });
});

describe("bps", () => {
  it.each([0, 1, 500, 1000, BPS_DENOMINATOR])("accepts %d", (n) => {
    expect(bps(n)).toBe(n);
  });
  it.each([-1, BPS_DENOMINATOR + 1, 2.5, Number.NaN])("rejects %d", (n) => {
    expect(() => bps(n)).toThrow(MoneyError);
  });
});

describe("currency", () => {
  it("recognises only known currencies", () => {
    expect(isCurrency("NGN")).toBe(true);
    expect(isCurrency("NGN_PLAY")).toBe(true);
    expect(isCurrency("USD")).toBe(false);
    expect(isCurrency(1)).toBe(false);
  });
});

describe("fees", () => {
  it("matches hand-computed values", () => {
    expect(feeOf(kobo(10_000), bps(1000))).toBe(1000);
    expect(feeOf(kobo(999), bps(1000))).toBe(99); // 99.9 floors to 99
    expect(feeOf(kobo(1), bps(1000))).toBe(0);
    expect(feeOf(kobo(0), bps(1000))).toBe(0);
  });

  it("rejects a negative amount", () => {
    expect(() => feeOf(kobo(-100), bps(1000))).toThrow(MoneyError);
  });

  it("stays exact where amount * rate exceeds 2^53", () => {
    // MAX * 9999 is far beyond 2^53. A float multiply would round here.
    const expected = (BigInt(MAX) * 9999n) / 10_000n;
    expect(BigInt(feeOf(kobo(MAX), bps(9999)))).toBe(expected);
  });
});

describe("splitPot properties", () => {
  const pot = fc.integer({ min: 0, max: MAX });
  const rate = fc.integer({ min: 0, max: BPS_DENOMINATOR });

  it("payout + fee === pot for every pot and rate", () => {
    fc.assert(
      fc.property(pot, rate, (p, r) => {
        const { payout, fee } = splitPot(kobo(p), bps(r));
        return payout + fee === p;
      }),
      { numRuns: 20_000 },
    );
  });

  it("the fee is exactly floor(pot * rate / 10000)", () => {
    fc.assert(
      fc.property(pot, rate, (p, r) => {
        const { fee } = splitPot(kobo(p), bps(r));
        return BigInt(fee) === (BigInt(p) * BigInt(r)) / 10_000n;
      }),
      { numRuns: 20_000 },
    );
  });

  it("the fee is within [0, pot] and the payout is never negative", () => {
    fc.assert(
      fc.property(pot, rate, (p, r) => {
        const { payout, fee } = splitPot(kobo(p), bps(r));
        return fee >= 0 && fee <= p && payout >= 0;
      }),
      { numRuns: 20_000 },
    );
  });

  it("a higher rate never yields a smaller fee", () => {
    fc.assert(
      fc.property(pot, rate, rate, (p, r1, r2) => {
        const [lo, hi] = r1 <= r2 ? [r1, r2] : [r2, r1];
        return feeOf(kobo(p), bps(lo)) <= feeOf(kobo(p), bps(hi));
      }),
      { numRuns: 20_000 },
    );
  });

  it("a zero rate takes nothing and a full rate takes everything", () => {
    fc.assert(
      fc.property(pot, (p) => {
        return (
          splitPot(kobo(p), bps(0)).payout === p && splitPot(kobo(p), bps(BPS_DENOMINATOR)).fee === p
        );
      }),
    );
  });
});
