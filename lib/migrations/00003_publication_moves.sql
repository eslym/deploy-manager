-- +goose Up
-- A destination is reserved before the filesystem mutation. Its inode and
-- source name survive process termination so recovery need not guess.
CREATE TABLE publication_moves (
 deployment_id TEXT PRIMARY KEY REFERENCES deployments(id) ON DELETE RESTRICT,
 source_name TEXT NOT NULL,
 destination_name TEXT NOT NULL UNIQUE,
 old_dev INTEGER NOT NULL,
 old_ino INTEGER NOT NULL,
 created_at INTEGER NOT NULL,
 CHECK(source_name <> destination_name)
);
-- Prevent a concurrent create (including older clients that omit the name)
-- from claiming a destination reserved by an unfinished move.
-- +goose StatementBegin
CREATE TRIGGER publication_move_reserve_insert BEFORE INSERT ON deployments
WHEN EXISTS (SELECT 1 FROM publication_moves WHERE destination_name=NEW.publication_name AND deployment_id<>NEW.id)
BEGIN SELECT RAISE(ABORT, 'publication destination reserved by move'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER publication_move_reserve_update BEFORE UPDATE OF publication_name ON deployments
WHEN EXISTS (SELECT 1 FROM publication_moves WHERE destination_name=NEW.publication_name AND deployment_id<>NEW.id)
BEGIN SELECT RAISE(ABORT, 'publication destination reserved by move'); END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER publication_move_reserve_update;
DROP TRIGGER publication_move_reserve_insert;
DROP TABLE publication_moves;
