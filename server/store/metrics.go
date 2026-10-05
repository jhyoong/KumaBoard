package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

// Metrics history is stored in two tiers: 30s samples for the recent past and
// 15-minute averages for long ranges.
const (
	// MetricsBucket is the resolution of the hot tier. Samples arriving
	// within the same bucket replace each other.
	MetricsBucket = 30 * time.Second

	// MetricsRollupBucket is the resolution of the rollup tier.
	MetricsRollupBucket = 15 * time.Minute

	// MetricsHotRetention is how long 30s samples are kept before
	// RollupMetrics folds them away.
	MetricsHotRetention = 2 * time.Hour

	// MetricsRetention is how long rolled-up history is kept.
	MetricsRetention = 30 * 24 * time.Hour
)

// MetricsSample is one stored history row, from either tier. GPUPercent is nil
// when the device reported no GPU utilisation, GPUMemUsedBytes when it
// reported no GPU memory, and GPUMemTotalBytes when the total is unknown.
// TempC is nil when the device reported no host temperature.
type MetricsSample struct {
	Bucket           time.Time
	CPUPercent       float64
	MemUsedBytes     uint64
	MemTotalBytes    uint64
	GPUPercent       *float64
	GPUMemUsedBytes  *uint64
	GPUMemTotalBytes *uint64
	DiskUsedPercent  float64
	TempC            *float64
}

// bucketOf returns the start of the bucket containing t, in unix seconds.
func bucketOf(t time.Time) int64 {
	return alignTo(t, MetricsBucket)
}

// rollupBucketOf returns the start of the rollup bucket containing t.
func rollupBucketOf(t time.Time) int64 {
	return alignTo(t, MetricsRollupBucket)
}

func alignTo(t time.Time, d time.Duration) int64 {
	b := int64(d / time.Second)
	u := t.Unix()
	return u - u%b
}

// gpuRollup is the busiest GPU's utilisation. A suspended GPU counts as idle;
// adapters reporting nothing are skipped. Nil when no adapter contributes.
func gpuRollup(gpus []proto.GPU) *float64 {
	var out *float64
	for _, g := range gpus {
		var v float64
		switch {
		case g.Suspended:
			v = 0
		case g.UtilPercent != nil:
			v = *g.UtilPercent
		default:
			continue
		}
		if out == nil || v > *out {
			out = &v
		}
	}
	return out
}

// gpuMemRollup sums GPU memory over the adapters reporting it, suspended ones
// included. used is nil when none report memory; total is nil unless every
// adapter reporting memory also reports a total.
func gpuMemRollup(gpus []proto.GPU) (used, total *int64) {
	var u, tot int64
	allTotals := true
	for _, g := range gpus {
		if g.MemUsedBytes == nil {
			continue
		}
		if used == nil {
			used = &u
		}
		u += int64(*g.MemUsedBytes)
		if g.MemTotalBytes == nil {
			allTotals = false
		} else {
			tot += int64(*g.MemTotalBytes)
		}
	}
	if used != nil && allTotals {
		total = &tot
	}
	return used, total
}

// RecordMetrics stores m in the bucket containing at, replacing any earlier
// sample for the same device and bucket.
func (s *Store) RecordMetrics(ctx context.Context, deviceID int64, at time.Time, m proto.Metrics) error {
	gpuUsed, gpuTotal := gpuMemRollup(m.GPUs)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO metrics_samples (device_id, bucket, cpu_percent, mem_used_bytes, mem_total_bytes, gpu_percent,
		   gpu_mem_used_bytes, gpu_mem_total_bytes, disk_used_percent, temp_c)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (device_id, bucket) DO UPDATE SET
		   cpu_percent = excluded.cpu_percent,
		   mem_used_bytes = excluded.mem_used_bytes,
		   mem_total_bytes = excluded.mem_total_bytes,
		   gpu_percent = excluded.gpu_percent,
		   gpu_mem_used_bytes = excluded.gpu_mem_used_bytes,
		   gpu_mem_total_bytes = excluded.gpu_mem_total_bytes,
		   disk_used_percent = excluded.disk_used_percent,
		   temp_c = excluded.temp_c`,
		deviceID, bucketOf(at), m.CPUPercent, int64(m.MemUsedBytes), int64(m.MemTotalBytes), gpuRollup(m.GPUs),
		gpuUsed, gpuTotal, m.DiskUsedPercent, m.TempC)
	return err
}

// MetricsHistory returns a device's 30s samples with buckets at or after
// since, oldest first.
func (s *Store) MetricsHistory(ctx context.Context, name string, since time.Time) ([]MetricsSample, error) {
	return s.queryHistory(ctx,
		`SELECT m.bucket, m.cpu_percent, m.mem_used_bytes, m.mem_total_bytes, m.gpu_percent,
		   m.gpu_mem_used_bytes, m.gpu_mem_total_bytes, m.disk_used_percent, m.temp_c
		 FROM metrics_samples m JOIN devices d ON d.id = m.device_id
		 WHERE d.name = ? AND m.bucket >= ? ORDER BY m.bucket`,
		name, bucketOf(since))
}

// rollupSelect averages 30s samples into rollup buckets (900s is
// MetricsRollupBucket). Callers append WHERE conditions on s before
// rollupGroup. AVG skips NULLs, so gpu is NULL only if no sample had a GPU
// (and likewise for the GPU memory and temperature columns).
const rollupSelect = `SELECT s.device_id, s.bucket - s.bucket % 900 AS rb, AVG(s.cpu_percent) AS cpu,
	CAST(AVG(s.mem_used_bytes) AS INTEGER) AS used, CAST(AVG(s.mem_total_bytes) AS INTEGER) AS total,
	AVG(s.gpu_percent) AS gpu, CAST(AVG(s.gpu_mem_used_bytes) AS INTEGER) AS gpu_used,
	CAST(AVG(s.gpu_mem_total_bytes) AS INTEGER) AS gpu_total, AVG(s.disk_used_percent) AS disk,
	AVG(s.temp_c) AS temp
	FROM metrics_samples s `

const rollupGroup = ` GROUP BY s.device_id, rb`

// MetricsRollupHistory returns a device's 15-minute history with buckets at or
// after since, oldest first. Buckets not yet in metrics_rollup (the recent
// past, including the one in progress) are averaged from the 30s tier on the
// fly so the series reaches the present.
func (s *Store) MetricsRollupHistory(ctx context.Context, name string, since time.Time) ([]MetricsSample, error) {
	from := rollupBucketOf(since)
	return s.queryHistory(ctx,
		`WITH dev AS (SELECT id FROM devices WHERE name = ?)
		 SELECT r.bucket, r.cpu_percent, r.mem_used_bytes, r.mem_total_bytes, r.gpu_percent,
		   r.gpu_mem_used_bytes, r.gpu_mem_total_bytes, r.disk_used_percent, r.temp_c
		 FROM metrics_rollup r WHERE r.device_id IN (SELECT id FROM dev) AND r.bucket >= ?
		 UNION ALL
		 SELECT rb, cpu, used, total, gpu, gpu_used, gpu_total, disk, temp FROM (`+rollupSelect+`WHERE s.device_id IN (SELECT id FROM dev) AND s.bucket >= ?`+rollupGroup+`)
		 WHERE rb NOT IN (SELECT bucket FROM metrics_rollup WHERE device_id IN (SELECT id FROM dev) AND bucket >= ?)
		 ORDER BY 1`,
		name, from, from, from)
}

func (s *Store) queryHistory(ctx context.Context, query string, args ...any) ([]MetricsSample, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricsSample{}
	for rows.Next() {
		var ms MetricsSample
		var bucket, used, total int64
		var gpu, temp sql.NullFloat64
		var gpuUsed, gpuTotal sql.NullInt64
		if err := rows.Scan(&bucket, &ms.CPUPercent, &used, &total, &gpu, &gpuUsed, &gpuTotal, &ms.DiskUsedPercent, &temp); err != nil {
			return nil, err
		}
		ms.Bucket = time.Unix(bucket, 0).UTC()
		ms.MemUsedBytes, ms.MemTotalBytes = uint64(used), uint64(total)
		if gpu.Valid {
			ms.GPUPercent = &gpu.Float64
		}
		if temp.Valid {
			ms.TempC = &temp.Float64
		}
		if gpuUsed.Valid {
			v := uint64(gpuUsed.Int64)
			ms.GPUMemUsedBytes = &v
		}
		if gpuTotal.Valid {
			v := uint64(gpuTotal.Int64)
			ms.GPUMemTotalBytes = &v
		}
		out = append(out, ms)
	}
	return out, rows.Err()
}

// RollupMetrics averages every completed 15-minute bucket of 30s samples into
// metrics_rollup (recomputing buckets already rolled up while their samples
// remain), then drops 30s samples older than MetricsHotRetention. The drop
// cutoff is rollup-aligned so a bucket's samples are never partially removed.
func (s *Store) RollupMetrics(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO metrics_rollup
		   (device_id, bucket, cpu_percent, mem_used_bytes, mem_total_bytes, gpu_percent,
		    gpu_mem_used_bytes, gpu_mem_total_bytes, disk_used_percent, temp_c) `+
			rollupSelect+`WHERE s.bucket < ?`+rollupGroup,
		rollupBucketOf(now)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metrics_samples WHERE bucket < ?`,
		rollupBucketOf(now.Add(-MetricsHotRetention))); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteMetricsBefore removes history from both tiers in buckets that start
// before t, returning the total rows removed.
func (s *Store) DeleteMetricsBefore(ctx context.Context, t time.Time) (int64, error) {
	var total int64
	for _, q := range []struct {
		sql    string
		bucket int64
	}{
		{`DELETE FROM metrics_samples WHERE bucket < ?`, bucketOf(t)},
		{`DELETE FROM metrics_rollup WHERE bucket < ?`, rollupBucketOf(t)},
	} {
		res, err := s.db.ExecContext(ctx, q.sql, q.bucket)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}
