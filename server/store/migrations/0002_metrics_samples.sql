-- One coalesced sample per device per 30-second bucket, for short-range
-- dashboard history. bucket is unix seconds, a multiple of 30. gpu_percent is
-- NULL when the device reported no GPU utilisation in that bucket.
CREATE TABLE metrics_samples (
    device_id       INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    bucket          INTEGER NOT NULL,
    cpu_percent     REAL NOT NULL,
    mem_used_bytes  INTEGER NOT NULL,
    mem_total_bytes INTEGER NOT NULL,
    gpu_percent     REAL,
    PRIMARY KEY (device_id, bucket)
) WITHOUT ROWID;

CREATE INDEX idx_metrics_samples_bucket ON metrics_samples(bucket);
