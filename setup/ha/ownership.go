package ha

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Activation names one authorized homeserver activation. Its generation is
// durable and monotonic, and it is recorded outside the Kubernetes Lease so
// deleting or recreating the Lease cannot reset ownership authority.
type Activation struct {
	Generation int64
	Identity   string
	Scope      string
	AcquiredAt time.Time
}

// ErrOwnershipRaced reports that another candidate advanced the generation
// first. The loser must never retry with its stale expectation.
var ErrOwnershipRaced = errors.New("HA ownership generation advanced by another candidate")

// ErrOwnershipAmbiguous reports that ownership history exists but the current
// record does not. Authority cannot be reconstructed from history alone, so a
// candidate must never treat this as a first bootstrap.
var ErrOwnershipAmbiguous = errors.New("HA ownership record is missing while ownership history exists")

// ErrOwnershipScope reports that the ownership record belongs to a different
// homeserver. Sharing one database between homeservers is unsupported and must
// fail closed rather than retire an unrelated deployment's generation.
var ErrOwnershipScope = errors.New("HA ownership record belongs to a different election scope")

// ownershipLockKey serializes ownership schema creation and generation
// advances. It is distinct from the migration lock taken by PrepareStorage.
const ownershipLockKey = 349

const ownershipSchema = `
CREATE TABLE IF NOT EXISTS zendrite_ha_ownership (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	generation BIGINT NOT NULL,
	owner TEXT NOT NULL,
	scope TEXT NOT NULL,
	acquired_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE IF NOT EXISTS zendrite_ha_ownership_history (
	generation BIGINT PRIMARY KEY,
	owner TEXT NOT NULL,
	scope TEXT NOT NULL,
	acquired_at TIMESTAMPTZ NOT NULL
);`

// ensureOwnershipSchema creates the ownership tables under an advisory lock.
// Concurrent CREATE TABLE IF NOT EXISTS statements race in PostgreSQL, and
// competing promoters reach this path at the same time by design.
func ensureOwnershipSchema(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, 1)", ownershipLockKey); err != nil {
		return fmt.Errorf("lock HA ownership record: %w", err)
	}
	if _, err := tx.Exec(ctx, ownershipSchema); err != nil {
		return fmt.Errorf("initialize HA ownership record: %w", err)
	}
	return nil
}

// firstBootstrap reports whether this database has never held an activation.
// An absent current record combined with existing history is ambiguous rather
// than new, and must fail closed.
func firstBootstrap(ctx context.Context, tx pgx.Tx) error {
	var retired int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM zendrite_ha_ownership_history").Scan(&retired); err != nil {
		return fmt.Errorf("read HA ownership history: %w", err)
	}
	if retired > 0 {
		return fmt.Errorf("%w: %d retired generation(s) recorded; restore the record or bootstrap offline", ErrOwnershipAmbiguous, retired)
	}
	return nil
}

// readOwnership reports the currently recorded activation. A database without
// an ownership record reports generation zero, which is the only value a first
// bootstrap may advance from.
func readOwnership(ctx context.Context, conn *pgx.Conn, scope string) (Activation, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return Activation{}, fmt.Errorf("begin HA ownership read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = ensureOwnershipSchema(ctx, tx); err != nil {
		return Activation{}, err
	}
	activation := Activation{Scope: scope}
	err = tx.QueryRow(ctx, "SELECT generation, owner, scope, acquired_at FROM zendrite_ha_ownership WHERE id = 1").
		Scan(&activation.Generation, &activation.Identity, &activation.Scope, &activation.AcquiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if err = firstBootstrap(ctx, tx); err != nil {
			return Activation{}, err
		}
		return Activation{Scope: scope}, nil
	}
	if err != nil {
		return Activation{}, fmt.Errorf("read HA ownership record: %w", err)
	}
	if activation.Scope != scope {
		return Activation{}, fmt.Errorf("%w: recorded %q, configured %q", ErrOwnershipScope, activation.Scope, scope)
	}
	return activation, nil
}

// advanceOwnership retires every earlier generation in one transaction. The
// conditional update serializes competing promoters, so a candidate holding a
// stale expectation loses instead of reusing a retired generation.
func advanceOwnership(ctx context.Context, conn *pgx.Conn, scope, identity string, expected int64) (Activation, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return Activation{}, fmt.Errorf("begin HA ownership advance: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = ensureOwnershipSchema(ctx, tx); err != nil {
		return Activation{}, err
	}
	// The row lock serializes promoters that read the same generation
	// concurrently; the conditional update below still rejects a stale caller.
	var observed int64
	var observedScope string
	err = tx.QueryRow(ctx, "SELECT generation, scope FROM zendrite_ha_ownership WHERE id = 1 FOR UPDATE").Scan(&observed, &observedScope)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err = firstBootstrap(ctx, tx); err != nil {
			return Activation{}, err
		}
		observedScope = scope
	case err != nil:
		return Activation{}, fmt.Errorf("lock HA ownership record: %w", err)
	}
	if observedScope != scope {
		return Activation{}, fmt.Errorf("%w: recorded %q, configured %q", ErrOwnershipScope, observedScope, scope)
	}
	if observed != expected {
		return Activation{}, fmt.Errorf("%w: expected generation %d, found %d", ErrOwnershipRaced, expected, observed)
	}
	activation := Activation{Generation: observed + 1, Identity: identity, Scope: scope, AcquiredAt: time.Now().UTC()}
	if _, err = tx.Exec(ctx,
		`INSERT INTO zendrite_ha_ownership (id, generation, owner, scope, acquired_at) VALUES (1, $1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET generation = $1, owner = $2, acquired_at = $4`,
		activation.Generation, activation.Identity, activation.Scope, activation.AcquiredAt); err != nil {
		return Activation{}, fmt.Errorf("advance HA ownership generation: %w", err)
	}
	if _, err = tx.Exec(ctx,
		"INSERT INTO zendrite_ha_ownership_history (generation, owner, scope, acquired_at) VALUES ($1, $2, $3, $4)",
		activation.Generation, activation.Identity, activation.Scope, activation.AcquiredAt); err != nil {
		return Activation{}, fmt.Errorf("record HA ownership history: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return Activation{}, fmt.Errorf("commit HA ownership advance: %w", err)
	}
	return activation, nil
}

// PostgreSQL truncates application_name to NAMEDATALEN-1 bytes, so the marker
// identifies an activation by short digests rather than by full identities.
const (
	applicationNamePrefix = "zendrite:"
	ownershipConnName     = "zendrite-ha-ownership"
	digestLength          = 8
)

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:digestLength]
}

// applicationName marks every application backend with its activation so a
// promoter can recognize and terminate the backends of earlier generations.
// The scope digest keeps an unrelated homeserver's backends out of the sweep.
func applicationName(activation Activation) string {
	return fmt.Sprintf("%s%s:%d:%s", applicationNamePrefix, digest(activation.Scope), activation.Generation, digest(activation.Identity))
}

// guardConnection refuses a connection that belongs to a retired activation.
// It runs on every new pooled connection, including reconnections, so a
// resumed owner cannot regain database access after a successor is promoted.
func guardConnection(activation Activation) func(context.Context, *pgx.Conn) error {
	return func(ctx context.Context, conn *pgx.Conn) error {
		var generation int64
		var scope string
		err := conn.QueryRow(ctx, "SELECT generation, scope FROM zendrite_ha_ownership WHERE id = 1").Scan(&generation, &scope)
		if err != nil {
			return fmt.Errorf("verify HA ownership generation: %w", err)
		}
		if scope != activation.Scope || generation != activation.Generation {
			return fmt.Errorf("HA ownership generation %d is retired; current generation is %d: refusing database access", activation.Generation, generation)
		}
		return nil
	}
}

// fenceBackends terminates every backend belonging to an earlier activation of
// this homeserver and reports how many it stopped. It must run only after the
// generation advance has committed: a backend that connected earlier is already
// visible here, and one that connects later is refused by guardConnection.
func fenceBackends(ctx context.Context, conn *pgx.Conn, activation Activation) (int, error) {
	const sweep = `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database()
		AND pid <> pg_backend_pid()
		AND application_name LIKE $1
		AND application_name <> $2`
	rows, err := conn.Query(ctx, sweep, applicationNamePrefix+digest(activation.Scope)+":%", applicationName(activation))
	if err != nil {
		return 0, fmt.Errorf("terminate retired HA backends: %w", err)
	}
	defer rows.Close()
	terminated := 0
	for rows.Next() {
		terminated++
	}
	if err = rows.Err(); err != nil {
		return terminated, fmt.Errorf("terminate retired HA backends: %w", err)
	}
	return terminated, nil
}

// retiredBackends counts backends of earlier activations that are still
// connected, so promotion can confirm a clean sweep before activating.
func retiredBackends(ctx context.Context, conn *pgx.Conn, activation Activation) (int, error) {
	const count = `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database()
		AND pid <> pg_backend_pid()
		AND application_name LIKE $1
		AND application_name <> $2`
	var remaining int
	if err := conn.QueryRow(ctx, count, applicationNamePrefix+digest(activation.Scope)+":%", applicationName(activation)).Scan(&remaining); err != nil {
		return 0, fmt.Errorf("count retired HA backends: %w", err)
	}
	return remaining, nil
}
