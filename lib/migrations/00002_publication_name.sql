-- +goose Up
-- Backfill in place: legacy live directories remain named by their IDs.
ALTER TABLE deployments ADD COLUMN publication_name TEXT NOT NULL DEFAULT '';
UPDATE deployments SET publication_name=id;
CREATE UNIQUE INDEX deployment_publication_name_unique ON deployments(publication_name) WHERE publication_name<>'';
-- Inserts from older local tooling that omit this field keep the old ID path.
-- +goose StatementBegin
CREATE TRIGGER deployments_legacy_publication_name AFTER INSERT ON deployments
WHEN NEW.publication_name=''
BEGIN
  UPDATE deployments SET publication_name=NEW.id WHERE id=NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER deployments_legacy_publication_name;
DROP INDEX deployment_publication_name_unique;
ALTER TABLE deployments DROP COLUMN publication_name;
