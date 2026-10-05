-- Host CPU/SoC temperature in °C. NULL when the device reported none
-- (Windows, sensorless hosts, pre-temperature agents). The sensor label is
-- live-only and not stored.
ALTER TABLE metrics_samples ADD COLUMN temp_c REAL;
ALTER TABLE metrics_rollup ADD COLUMN temp_c REAL;
