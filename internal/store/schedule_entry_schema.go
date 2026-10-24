package store

// Provisional next migration; allocate/renumber in merge order before release.
const scheduleEntryMigrationVersion = 31
const scheduleEntrySchema = `
ALTER TABLE runs ADD COLUMN schedule_entry TEXT NOT NULL DEFAULT ''
    CHECK(length(CAST(schedule_entry AS BLOB)) <= 64
      AND (schedule_entry = '' OR (trigger = 'schedule' AND ref_kind = 'branch' AND release = 0)));
CREATE TRIGGER run_schedule_entry_immutable BEFORE UPDATE OF schedule_entry ON runs
WHEN NEW.schedule_entry IS NOT OLD.schedule_entry
BEGIN SELECT RAISE(ABORT, 'run schedule entry is immutable'); END;
`
