package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const maxAttempts = 3

type job struct {
	ID        string  `json:"task_id"`
	Status    string  `json:"status"`
	ErrorCode *string `json:"error_code,omitempty"`
	result    *string
}

func (s *service) findJob(ctx context.Context, owner, id string) (job, error) {
	j := job{ID: id}
	err := s.db.QueryRow(ctx, "SELECT status,error_code,result_key FROM image_jobs WHERE id=$1::text::uuid AND owner=$2", id, owner).Scan(&j.Status, &j.ErrorCode, &j.result)
	return j, err
}

func (s *service) byKey(ctx context.Context, owner, key string) (id, hash string, err error) {
	err = s.db.QueryRow(ctx, "SELECT id::text,request_hash FROM image_jobs WHERE owner=$1 AND idempotency_key=$2", owner, key).Scan(&id, &hash)
	return
}

func (s *service) createJob(ctx context.Context, id, owner, filter, source, hash, key string) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO image_jobs(id,owner,filter,source_key,request_hash,idempotency_key,status)
 VALUES($1::text::uuid,$2,$3,$4,$5,NULLIF($6,''),'queued') ON CONFLICT(owner,idempotency_key) DO NOTHING`, id, owner, filter, source, hash, key)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO image_outbox(job_id) VALUES($1::text::uuid)", id); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

type attempt struct {
	id, token, filter, source string
	count                     int
	created                   time.Time
}

func (s *service) claim(ctx context.Context, id, token string) (attempt, error) {
	a := attempt{id: id, token: token}
	err := s.db.QueryRow(ctx, `UPDATE image_jobs SET status='processing', attempts=attempts+1, lease_token=$2::text::uuid,
 lease_until=now()+make_interval(secs=>$3) WHERE id=$1::text::uuid AND status='queued' AND attempts<$4
 AND EXISTS (SELECT 1 FROM image_outbox WHERE job_id=image_jobs.id AND available_at<=now())
 RETURNING filter,source_key,attempts,created_at`, id, token, s.lease.Seconds(), maxAttempts).Scan(&a.filter, &a.source, &a.count, &a.created)
	return a, err
}

func (s *service) finish(ctx context.Context, a attempt, status, result, code string) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE image_jobs SET status=$3,result_key=NULLIF($4,''),error_code=NULLIF($5,''),
 lease_token=NULL,lease_until=NULL WHERE id=$1::text::uuid AND lease_token=$2::text::uuid AND lease_until>now()`, a.id, a.token, status, result, code)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	if status == "queued" {
		_, err = tx.Exec(ctx, `UPDATE image_outbox SET published_at=NULL,available_at=now()+make_interval(secs=>$2) WHERE job_id=$1::text::uuid`, a.id, float64(int64(1)<<uint(a.count)))
		if err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func (s *service) recoverExpired(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `WITH expired AS (
 UPDATE image_jobs SET status=CASE WHEN attempts >= $1 THEN 'failed' ELSE 'queued' END,
 error_code=CASE WHEN attempts >= $1 THEN 'attempts_exhausted' ELSE NULL END,
 lease_token=NULL,lease_until=NULL WHERE status='processing' AND lease_until<=now()
 RETURNING id,status)
 UPDATE image_outbox SET published_at=NULL,available_at=now() FROM expired
 WHERE job_id=expired.id AND expired.status='queued'`, maxAttempts)
	return err
}

func missing(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
