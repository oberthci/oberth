package store

const parkPromotionMigrationVersion = 54
const parkPromotionSchema = `
ALTER TABLE promotions ADD COLUMN branch_ci_skipped INTEGER NOT NULL DEFAULT 0 CHECK (branch_ci_skipped IN (0,1));
CREATE TRIGGER promotions_branch_ci_immutable
BEFORE UPDATE OF branch_ci_skipped ON promotions
WHEN NEW.branch_ci_skipped != OLD.branch_ci_skipped
BEGIN SELECT RAISE(ABORT, 'promotion branch CI evidence is immutable'); END;
`
