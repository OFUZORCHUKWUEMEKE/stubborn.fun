# Stubborn

Head-to-head games for real stakes, iMessage-games style: challenge a friend
to Wordle or Ultimate X-and-O, both lock a stake, the winner takes the pot
less a platform fee. A web app (PWA), built entirely on Cloudflare Workers
and Durable Objects, in TypeScript.

## Architecture

```
             ┌─────────── PWA · Workers Static Assets ───────────┐
             │ React/Vite · shared rules engine for instant UI    │
             └────────┬────────────────────────────┬─────────────┘
                 HTTPS│                   WebSocket│
             ┌────────▼────────────────────────────▼─────────────┐
             │ edge Worker (Hono): auth · Turnstile · rate limit  │
             └──┬──────────────┬──────────────┬───────────────┬──┘
         ┌──────▼─────┐ ┌──────▼─────┐ ┌──────▼─────┐   ┌─────▼─────┐
         │ GameRoom   │ │ Matchmaker │ │ Player     │   │ D1: users,│
         │ 1 per game │ │ 1 per tier │ │ 1 per user │   │ sessions, │
         └──────┬─────┘ └────────────┘ └────────────┘   │ history   │
                │ fixed ledger calls (op ids + outbox)  └───────────┘
   ┌────────────▼──────────────────────┐
   │ ledger Worker — NO public route   │──► Queue ──► R2 (locked audit log)
   │ Ledger Durable Object · SQLite    │
   └────────────▲──────────────────────┘
                │ fixed ledger calls
   ┌────────────┴──────────────────────┐
   │ payments Worker                   │◄── payment provider webhooks
   │ Workflows: deposit · withdrawal   │
   └───────────────────────────────────┘
```

Three ideas carry the design:

1. **Every game is its own small server.** One `GameRoom` Durable Object per
   game holds the board, validates every move, runs the clocks, and owns
   the WebSockets. Clients are never trusted with game state.
2. **Money lives in exactly one place, and the internet cannot reach it.**
   All balances are in a Ledger Durable Object inside a Worker with no
   public route. Other Workers reach it only through a service binding to a
   fixed set of operations.
3. **Rules code runs the same on the phone and the server.** Game rules are
   pure TypeScript shared by the PWA (instant feedback) and the `GameRoom`
   (the authority).

### The ledger

- **Double-entry, one SQLite transaction per operation.** A Durable Object
  handles one event at a time and no operation awaits anything, so money
  operations are fully serialised: there are no check-then-act races.
- **The schema enforces the invariants itself.** Balances move only through
  a trigger on entry insert; a `CHECK` refuses any non-clearing balance
  below zero; entries and ops are append-only (triggers abort `UPDATE` and
  `DELETE`); `UNIQUE` constraints rule out double staking and duplicate
  operations.
- **Idempotent on caller op ids.** Replaying an op returns its original
  result; reusing an op id for a different request is an
  `IDEMPOTENCY_CONFLICT`; failed ops leave no trace and can be retried.
- **One instance per currency.** Real money (`NGN`) and play money
  (`NGN_PLAY`) live in separate databases; each instance refuses any other
  currency.
- **Game settlement is exact.** `fee = floor(pot × bps / 10000)` and the
  winner gets `pot − fee`, so the two always sum to the pot. The fee rate is
  fixed per game when its first stake is locked. Draws and abandoned games
  refund every stake in full.
- **Operations:** `deposit`, `lockStake`, `settleGame`, `refundGame`,
  `holdWithdrawal`, `completeWithdrawal`, `reverseWithdrawal`, plus reads and
  `reconcile`, which re-derives every invariant from the raw tables.

## Layout

```
apps/ledger      private Worker: Ledger Durable Object + LedgerService entrypoint
packages/money   Kobo (exact integer) and basis-point types, fee math
```

Planned: `apps/edge` (public API, GameRoom/Matchmaker/Player objects),
`apps/payments` (webhooks, Workflows), `apps/web` (PWA), `packages/games`
(rules engines), `packages/protocol` (shared message schemas).

## Development

Requires Node 22+ and pnpm 10.

```sh
pnpm install
pnpm test        # every package; ledger tests run inside workerd on real DO SQLite
pnpm typecheck
```

Ledger tests use `@cloudflare/vitest-plugin`, so they exercise the real
Workers runtime and real Durable Object SQLite storage rather than mocks.

## Before real money goes live

Engineering does not settle these:

- **Licensing.** Since the Supreme Court's 22 November 2024 ruling, gaming
  is regulated by the states (in Lagos, the Lagos State Lotteries and Gaming
  Authority). How that applies to online platforms that cross state lines is
  unsettled. Get a Nigerian gaming lawyer's written opinion.
- **Payment providers.** Paystack's policy lists gambling — explicitly
  including games of skill with an entry fee and a prize — as ineligible
  without prior approval. Disclose the business accurately; a provider that
  discovers an undisclosed gaming merchant can freeze settlements, which
  here are users' deposits.
- **Abuse.** Chip dumping (deliberately losing to move money) is the main
  abuse in real-money head-to-head play; withdrawals require verified
  identity and payout only to a bank account in that name.

The currency tag makes a play-money beta (`NGN_PLAY`) available while those
are resolved.
