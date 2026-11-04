package store

const promotionTrainMigrationVersion = 53
const promotionTrainSchema = `
ALTER TABLE promotions ADD COLUMN train_json TEXT NOT NULL DEFAULT '' CHECK(train_json = '' OR json_valid(train_json));
ALTER TABLE promotions ADD COLUMN train_key TEXT NOT NULL DEFAULT '';
CREATE TABLE promotion_train_grants (
 promotion_id TEXT NOT NULL REFERENCES promotions(id) ON DELETE CASCADE,
 grant_id TEXT NOT NULL REFERENCES promotion_grants(id),
 included INTEGER NOT NULL DEFAULT 1 CHECK(included IN (0,1)),
 PRIMARY KEY(promotion_id, grant_id)
);
CREATE INDEX promotion_train_grants_holder_idx ON promotion_train_grants(grant_id, promotion_id);
CREATE TRIGGER promotion_train_identity_immutable BEFORE UPDATE ON promotions
WHEN NEW.train_key != OLD.train_key OR (NEW.train_json != OLD.train_json AND
 (OLD.status != 'pending' OR OLD.result_sha != '' OR NEW.status != 'pending'))
BEGIN SELECT RAISE(ABORT, 'promotion train plan is immutable'); END;
`
