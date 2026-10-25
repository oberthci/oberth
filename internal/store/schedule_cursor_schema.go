package store

// Migration v32 follows the schedule-entry identity migration v31.
const scheduleCursorMigrationVersion = 32
const scheduleCursorSchema = `
ALTER TABLE schedule_fires ADD COLUMN last_fired_at INTEGER;
UPDATE schedule_fires SET last_fired_at = CASE
    WHEN outcome = 'fired' THEN fired_at
    WHEN outcome IN ('skipped', 'failed', 'paused') AND entry != '' THEN 0
    ELSE NULL END;
`
