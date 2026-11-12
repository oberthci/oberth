-- Purpose: persist the forge target observed before a promotion passes (#1541).
-- Rollback: Restore the pre-upgrade database backup.
-- Existing promotions remain unconfirmed; never infer a historical observation.
ALTER TABLE promotions ADD COLUMN observed_target_sha TEXT NOT NULL DEFAULT ''
 CHECK(observed_target_sha = '' OR (status = 'passed' AND observed_target_sha = result_sha));
CREATE TRIGGER promotions_confirmation_guard
BEFORE UPDATE ON promotions
WHEN NEW.observed_target_sha != OLD.observed_target_sha AND NOT (
 OLD.status = 'pending' AND OLD.observed_target_sha = '' AND NEW.status = 'passed'
 AND NEW.observed_target_sha = NEW.result_sha AND NEW.result_sha != ''
)
BEGIN
 SELECT RAISE(ABORT, 'promotion forge confirmation is immutable');
END;
