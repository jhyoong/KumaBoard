-- Disk usage is captured in the 30s tier so it can be rolled up.
ALTER TABLE metrics_samples ADD COLUMN disk_used_percent REAL NOT NULL DEFAULT 0;

-- Long-range history: the average of a device's 30s samples per 15-minute
-- bucket. bucket is unix seconds, a multiple of 900. gpu_percent is NULL when
-- no sample in the bucket reported GPU utilisation.
CREATE TABLE metrics_rollup (
    device_id         INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    bucket            INTEGER NOT NULL,
    cpu_percent       REAL NOT NULL,
    mem_used_bytes    INTEGER NOT NULL,
    mem_total_bytes   INTEGER NOT NULL,
    gpu_percent       REAL,
    disk_used_percent REAL NOT NULL,
    PRIMARY KEY (device_id, bucket)
) WITHOUT ROWID;

CREATE INDEX idx_metrics_rollup_bucket ON metrics_rollup(bucket);
