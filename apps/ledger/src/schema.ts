/**
 * Ledger schema. The database enforces the money invariants itself, so a
 * bug in application code is refused by SQLite rather than written:
 *
 *  - Balances are maintained only by a trigger on entry insert. Code never
 *    updates a balance directly, so a balance can never drift from its
 *    entries through a forgotten write.
 *  - A CHECK constraint refuses any non-clearing balance below zero. The
 *    application checks funds first to return a clear error; this is the
 *    backstop if that check is ever wrong.
 *  - Entries and ops are append-only: triggers abort any UPDATE or DELETE.
 *    Corrections are new offsetting entries, never edits.
 *  - UNIQUE constraints make double staking, duplicate withdrawals, and
 *    duplicate operation ids impossible at the storage level.
 */
export const SCHEMA_VERSION = 1;

export const SCHEMA_STATEMENTS: readonly string[] = [
  `CREATE TABLE IF NOT EXISTS meta (
     key   TEXT PRIMARY KEY,
     value TEXT NOT NULL
   )`,

  `CREATE TABLE IF NOT EXISTS accounts (
     id             INTEGER PRIMARY KEY,
     ref            TEXT    NOT NULL,
     currency       TEXT    NOT NULL,
     balance        INTEGER NOT NULL DEFAULT 0,
     allow_negative INTEGER NOT NULL DEFAULT 0 CHECK (allow_negative IN (0, 1)),
     created_at     INTEGER NOT NULL,
     UNIQUE (ref, currency),
     CHECK (allow_negative = 1 OR balance >= 0)
   )`,

  `CREATE TABLE IF NOT EXISTS ops (
     op_id      TEXT PRIMARY KEY,
     kind       TEXT    NOT NULL,
     request    TEXT    NOT NULL,
     result     TEXT    NOT NULL,
     created_at INTEGER NOT NULL
   )`,

  `CREATE TABLE IF NOT EXISTS entries (
     id         INTEGER PRIMARY KEY,
     op_id      TEXT    NOT NULL,
     account_id INTEGER NOT NULL REFERENCES accounts (id),
     amount     INTEGER NOT NULL CHECK (amount <> 0),
     created_at INTEGER NOT NULL
   )`,
  `CREATE INDEX IF NOT EXISTS entries_by_account ON entries (account_id)`,
  `CREATE INDEX IF NOT EXISTS entries_by_op ON entries (op_id)`,

  `CREATE TABLE IF NOT EXISTS escrows (
     game_id      TEXT    PRIMARY KEY,
     currency     TEXT    NOT NULL,
     account_id   INTEGER NOT NULL REFERENCES accounts (id),
     fee_bps      INTEGER NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
     status       TEXT    NOT NULL CHECK (status IN ('open', 'settled', 'refunded')),
     closed_by_op TEXT,
     created_at   INTEGER NOT NULL
   )`,

  `CREATE TABLE IF NOT EXISTS stakes (
     game_id    TEXT    NOT NULL REFERENCES escrows (game_id),
     user_id    TEXT    NOT NULL,
     amount     INTEGER NOT NULL CHECK (amount > 0),
     op_id      TEXT    NOT NULL,
     created_at INTEGER NOT NULL,
     PRIMARY KEY (game_id, user_id)
   )`,

  `CREATE TABLE IF NOT EXISTS withdrawals (
     withdrawal_id TEXT    PRIMARY KEY,
     user_id       TEXT    NOT NULL,
     currency      TEXT    NOT NULL,
     amount        INTEGER NOT NULL CHECK (amount > 0),
     provider      TEXT    NOT NULL,
     status        TEXT    NOT NULL CHECK (status IN ('held', 'completed', 'reversed')),
     closed_by_op  TEXT,
     created_at    INTEGER NOT NULL
   )`,

  // Balances move only here, in the same statement as the entry insert.
  // If the update violates the balance CHECK, the insert fails with it.
  `CREATE TRIGGER IF NOT EXISTS entries_apply_balance
     AFTER INSERT ON entries
   BEGIN
     UPDATE accounts SET balance = balance + NEW.amount WHERE id = NEW.account_id;
   END`,

  `CREATE TRIGGER IF NOT EXISTS entries_no_update
     BEFORE UPDATE ON entries
   BEGIN
     SELECT RAISE(ABORT, 'ledger entries are append-only');
   END`,
  `CREATE TRIGGER IF NOT EXISTS entries_no_delete
     BEFORE DELETE ON entries
   BEGIN
     SELECT RAISE(ABORT, 'ledger entries are append-only');
   END`,
  `CREATE TRIGGER IF NOT EXISTS ops_no_update
     BEFORE UPDATE ON ops
   BEGIN
     SELECT RAISE(ABORT, 'ledger ops are append-only');
   END`,
  `CREATE TRIGGER IF NOT EXISTS ops_no_delete
     BEFORE DELETE ON ops
   BEGIN
     SELECT RAISE(ABORT, 'ledger ops are append-only');
   END`,
];
