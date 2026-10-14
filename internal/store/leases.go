package store

import (
	"database/sql"
	"math"
	"strconv"
	"strings"
	"time"
)

const DefaultLeaseTTL = time.Hour

type Lease struct {
	Resource   string `json:"resource"`
	Worker     string `json:"worker"`
	HolderName string `json:"holder_name,omitempty"`
	AcquiredAt int64  `json:"acquired_at"`
	ExpiresAt  int64  `json:"expires_at"`
	Scope      string `json:"scope"`
	DispatchID string `json:"dispatch_id,omitempty"`
}

// NormalizeResource gives aliases for the same port or simulator one lock name.
func NormalizeResource(resource string) (string, error) {
	kind, name, ok := strings.Cut(resource, ":")
	if !ok || name == "" || len(name) > 200 {
		return "", refuse("bad_resource", "resource must be sim:<udid>|port:<n>|db:<name>")
	}
	switch kind {
	case "port":
		for _, c := range name {
			if c < '0' || c > '9' {
				return "", refuse("bad_resource", "port must be an integer from 1 to 65535")
			}
		}
		n, err := strconv.Atoi(name)
		if err != nil || n < 1 || n > 65535 {
			return "", refuse("bad_resource", "port must be an integer from 1 to 65535")
		}
		name = strconv.Itoa(n)
	case "sim", "db":
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				return "", refuse("bad_resource", "resource name must contain only letters, digits, dot, underscore or hyphen")
			}
		}
		if kind == "sim" {
			name = strings.ToLower(name)
		}
	default:
		return "", refuse("bad_resource", "resource must be sim:<udid>|port:<n>|db:<name>")
	}
	return kind + ":" + name, nil
}

func leaseExpiry(now int64, ttl time.Duration) (int64, error) {
	ms := ttl.Milliseconds()
	if ms <= 0 || now > math.MaxInt64-ms {
		return 0, refuse("bad_ttl", "lease TTL must be positive and at least 1ms")
	}
	return now + ms, nil
}

func expireLeases(q querier, now int64) error {
	_, err := q.Exec(`DELETE FROM lease_registry.leases WHERE expires_at<=?`, now)
	return err
}
func leaseWorker(q querier, pane string) error {
	var state, phase string
	err := q.QueryRow(`SELECT state,lifecycle FROM workers WHERE pane_id=?`, pane).Scan(&state, &phase)
	if err == sql.ErrNoRows {
		return refuse("unknown_worker", "pane %s is not a registered worker", pane)
	}
	if err != nil {
		return err
	}
	if phase != "" {
		return refuse("worker_lifecycle_busy", "worker %s is held for a lifecycle operation", pane)
	}
	if state != WorkerLive {
		return refuse("worker_not_live", "worker %s is %s", pane, state)
	}
	return nil
}
func acquireLease(q querier, resource, pane, scope, dispatch string, now, expires int64, renew bool) error {
	var holder, name, holderScope string
	err := q.QueryRow(`SELECT worker,holder_name,scope FROM lease_registry.leases WHERE resource=?`, resource).Scan(&holder, &name, &holderScope)
	if err == nil {
		if holder != pane || holderScope != scope {
			if name == "" {
				name = holder
			}
			return refuse("lease_held", "resource %s is held by %s (%s, session %s)", resource, name, holder, holderScope)
		}
		if renew {
			_, err = q.Exec(`UPDATE lease_registry.leases SET expires_at=?,dispatch_id=NULL WHERE resource=?`, expires, resource)
		}
		return err
	}
	if err != sql.ErrNoRows {
		return err
	}
	if err := q.QueryRow(`SELECT COALESCE(name,'') FROM workers WHERE pane_id=?`, pane).Scan(&name); err != nil {
		return err
	}
	_, err = q.Exec(`INSERT INTO lease_registry.leases(resource,worker,holder_name,scope,acquired_at,expires_at,dispatch_id) VALUES(?,?,?,?,?,?,?)`, resource, pane, name, scope, now, expires, nullStr(dispatch))
	return err
}

func (s *Store) AcquireLease(resource, pane string, ttl time.Duration) (Lease, error) {
	resource, err := NormalizeResource(resource)
	if err != nil {
		return Lease{}, err
	}
	now := s.now()
	expires, err := leaseExpiry(now, ttl)
	if err != nil {
		return Lease{}, err
	}
	var lease Lease
	err = s.tx(func(tx *sql.Tx) error {
		if err := expireLeases(tx, now); err != nil {
			return err
		}
		if err := leaseWorker(tx, pane); err != nil {
			return err
		}
		if err := acquireLease(tx, resource, pane, s.leaseScope, "", now, expires, true); err != nil {
			return err
		}
		var err error
		lease, err = scanLease(tx.QueryRow(`SELECT `+leaseCols+` FROM lease_registry.leases WHERE resource=?`, resource))
		return err
	})
	if err != nil {
		return Lease{}, err
	}
	return lease, nil
}

const leaseCols = `resource,worker,holder_name,acquired_at,expires_at,scope,COALESCE(dispatch_id,'')`

func scanLease(r interface{ Scan(...any) error }) (Lease, error) {
	var l Lease
	err := r.Scan(&l.Resource, &l.Worker, &l.HolderName, &l.AcquiredAt, &l.ExpiresAt, &l.Scope, &l.DispatchID)
	return l, err
}
func (s *Store) ListLeases() ([]Lease, error) {
	rows, err := s.db.Query(`SELECT `+leaseCols+` FROM lease_registry.leases WHERE expires_at>? ORDER BY resource`, s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Lease{}
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (s *Store) ReleaseLease(resource, pane string) error {
	resource, err := NormalizeResource(resource)
	if err != nil {
		return err
	}
	return s.tx(func(tx *sql.Tx) error {
		if err := expireLeases(tx, s.now()); err != nil {
			return err
		}
		var holder, name, scope string
		err := tx.QueryRow(`SELECT worker,holder_name,scope FROM lease_registry.leases WHERE resource=?`, resource).Scan(&holder, &name, &scope)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if holder != pane || scope != s.leaseScope {
			if name == "" {
				name = holder
			}
			return refuse("lease_held", "resource %s is held by %s (%s, session %s)", resource, name, holder, scope)
		}
		_, err = tx.Exec(`DELETE FROM lease_registry.leases WHERE resource=? AND worker=? AND scope=?`, resource, pane, s.leaseScope)
		return err
	})
}

// ReleaseWorkerWithLeases must only be called after process release is verified.
// Persist the released state and free resources together, never on a failed close.
func (s *Store) ReleaseWorkerWithLeases(pane, note, archive string, handOffTo ...string) error {
	if len(handOffTo) > 1 {
		return refuse("bad_args", "at most one lease hand-off recipient is allowed")
	}
	target := ""
	if len(handOffTo) == 1 {
		target = handOffTo[0]
	}
	if target == pane {
		return refuse("bad_args", "worker cannot hand leases to itself")
	}
	return s.tx(func(tx *sql.Tx) error {
		name := ""
		if target != "" {
			var state, phase string
			err := tx.QueryRow(`SELECT COALESCE(name,''),state,lifecycle FROM workers WHERE pane_id=?`, target).Scan(&name, &state, &phase)
			if err == sql.ErrNoRows {
				return refuse("unknown_worker", "hand-off recipient %s is not registered", target)
			}
			if err != nil {
				return err
			}
			if state != WorkerLive {
				return refuse("worker_not_live", "hand-off recipient %s is %s", target, state)
			}
			if phase != "" && phase != "receiving_hand_off" {
				return refuse("worker_lifecycle_busy", "hand-off recipient %s is held for %s", target, phase)
			}
		}
		res, err := tx.Exec(`UPDATE workers SET state=?,note=COALESCE(?,note),archive_path=COALESCE(?,archive_path),released_at=?,lifecycle='' WHERE pane_id=?`, WorkerReleased, nullStr(note), nullStr(archive), s.now(), pane)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return refuse("unknown_worker", "pane %s is not a registered worker", pane)
		}
		if target != "" {
			// A pane's verified hand-off directory may differ from its earlier
			// registration. Persist responsibility before a later sweep or release.
			if _, err = tx.Exec(`UPDATE workers SET worktree=(SELECT hand_off_worktree FROM workers WHERE pane_id=?) WHERE pane_id=? AND EXISTS (SELECT 1 FROM workers WHERE pane_id=? AND hand_off_to=? AND COALESCE(hand_off_worktree,'')<>'')`, pane, target, pane, target); err != nil {
				return err
			}
			_, err = tx.Exec(`UPDATE lease_registry.leases SET worker=?,holder_name=?,dispatch_id=NULL WHERE worker=? AND scope=?`, target, name, pane, s.leaseScope)
		} else {
			_, err = tx.Exec(`DELETE FROM lease_registry.leases WHERE worker=? AND scope=?`, pane, s.leaseScope)
		}
		return err
	})
}
