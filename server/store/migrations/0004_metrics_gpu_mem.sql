-- GPU memory history. gpu_mem_used_bytes is the summed memory of every GPU
-- reporting it and is NULL when none did. gpu_mem_total_bytes is the summed
-- total, NULL unless every GPU reporting memory also reported a total (Apple
-- unified memory has none).
ALTER TABLE metrics_samples ADD COLUMN gpu_mem_used_bytes INTEGER;
ALTER TABLE metrics_samples ADD COLUMN gpu_mem_total_bytes INTEGER;
ALTER TABLE metrics_rollup ADD COLUMN gpu_mem_used_bytes INTEGER;
ALTER TABLE metrics_rollup ADD COLUMN gpu_mem_total_bytes INTEGER;
