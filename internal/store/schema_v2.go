package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/model"
)

type migrationFunc func(context.Context, *sql.Tx, *model.AuditHead) error

type legacyAuditQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type legacySchemaQuerier interface {
	legacyAuditQuerier
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type legacyAuditVisitor func(int64, model.AuditActionSpec, int64, []byte, []byte) error

type legacySchemaObject struct {
	kind       string
	name       string
	table      string
	definition string
}

const createAuditChainV2Table = `
CREATE TABLE audit_actions_v2 (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    details TEXT NOT NULL,
    previous_sha256 BLOB NOT NULL CHECK (length(previous_sha256) = 32),
    sha256 BLOB NOT NULL UNIQUE CHECK (length(sha256) = 32),
    created_at INTEGER NOT NULL
)`

const finishAuditChainV2Schema = `
DROP TRIGGER audit_actions_no_update;
DROP TRIGGER audit_actions_no_delete;
DROP INDEX audit_actions_page_idx;
DROP TABLE audit_actions;
ALTER TABLE audit_actions_v2 RENAME TO audit_actions;
CREATE INDEX audit_actions_page_idx ON audit_actions(id DESC);

CREATE TRIGGER audit_actions_no_update
BEFORE UPDATE ON audit_actions
BEGIN
    SELECT RAISE(ABORT, 'audit actions are append-only');
END;
CREATE TRIGGER audit_actions_no_delete
BEFORE DELETE ON audit_actions
BEGIN
    SELECT RAISE(ABORT, 'audit actions are append-only');
END;

CREATE TABLE audit_anchors (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    audit_id INTEGER NOT NULL CHECK (audit_id >= 0),
    audit_sha256 BLOB NOT NULL CHECK (length(audit_sha256) = 32),
    tsa_url TEXT NOT NULL,
    receipt BLOB NOT NULL CHECK (length(receipt) BETWEEN 1 AND 4194304),
    anchored_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX audit_anchors_latest_idx ON audit_anchors(id DESC);
CREATE TRIGGER audit_anchors_no_update
BEFORE UPDATE ON audit_anchors
BEGIN
    SELECT RAISE(ABORT, 'audit anchors are append-only');
END;
CREATE TRIGGER audit_anchors_no_delete
BEFORE DELETE ON audit_anchors
BEGIN
    SELECT RAISE(ABORT, 'audit anchors are append-only');
END;
`

// migrateAuditChainV2 preserves every v1 audit record while making its order
// cryptographically explicit. A gap or malformed historical record fails the
// upgrade instead of silently defining a different chain.
func migrateAuditChainV2(ctx context.Context, tx *sql.Tx, expectedHead *model.AuditHead) error {
	if _, err := tx.ExecContext(ctx, createAuditChainV2Table); err != nil {
		return fmt.Errorf("create chained audit table: %w", err)
	}
	head, err := walkLegacyAuditActions(ctx, tx, func(id int64, spec model.AuditActionSpec, createdAt int64, previous, digest []byte) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO audit_actions_v2(
    id, actor, action, resource_type, resource_id, details, previous_sha256, sha256, created_at
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, spec.Actor, spec.Action, spec.ResourceType,
			spec.ResourceID, spec.Details, previous, digest, createdAt)
		if err != nil {
			return fmt.Errorf("chain v1 audit action %d: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if expectedHead != nil && (head.ID != expectedHead.ID || !bytes.Equal(head.SHA256, expectedHead.SHA256)) {
		return fmt.Errorf("%w: live v1 audit head changed after immutable continuity intent", ErrSchemaIncompatible)
	}

	if _, err := tx.ExecContext(ctx, finishAuditChainV2Schema); err != nil {
		return fmt.Errorf("activate chained audit schema: %w", err)
	}
	return nil
}

func walkLegacyAuditActions(ctx context.Context, querier legacyAuditQuerier, visit legacyAuditVisitor) (model.AuditHead, error) {
	rows, err := querier.QueryContext(ctx, `
SELECT id, actor, action, resource_type, resource_id, details, created_at
FROM audit_actions ORDER BY id`)
	if err != nil {
		return model.AuditHead{}, fmt.Errorf("read v1 audit actions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	expectedID := int64(1)
	previous := append([]byte(nil), auditGenesisSHA256...)
	head := model.AuditHead{SHA256: append([]byte(nil), auditGenesisSHA256...)}
	for rows.Next() {
		var id, createdAt int64
		var spec model.AuditActionSpec
		if err := rows.Scan(&id, &spec.Actor, &spec.Action, &spec.ResourceType, &spec.ResourceID, &spec.Details, &createdAt); err != nil {
			return model.AuditHead{}, fmt.Errorf("scan v1 audit action %d: %w", expectedID, err)
		}
		if id != expectedID {
			return model.AuditHead{}, fmt.Errorf("%w: v1 audit action sequence jumps from %d to %d", ErrSchemaIncompatible, expectedID-1, id)
		}
		if strings.TrimSpace(spec.Actor) == "" || strings.TrimSpace(spec.Action) == "" ||
			strings.TrimSpace(spec.ResourceType) == "" || !json.Valid([]byte(spec.Details)) || createdAt <= 0 {
			return model.AuditHead{}, fmt.Errorf("%w: v1 audit action %d is malformed", ErrSchemaIncompatible, id)
		}
		digest := auditActionSHA256(id, previous, spec, createdAt)
		if visit != nil {
			if err := visit(id, spec, createdAt, previous, digest); err != nil {
				return model.AuditHead{}, err
			}
		}
		previous = append([]byte(nil), digest...)
		head = model.AuditHead{ID: id, SHA256: append([]byte(nil), digest...)}
		expectedID++
	}
	if err := rows.Err(); err != nil {
		return model.AuditHead{}, fmt.Errorf("iterate v1 audit actions: %w", err)
	}
	return head, nil
}

func verifyLegacyV1Schema(ctx context.Context, querier legacySchemaQuerier) error {
	want, err := canonicalLegacyV1Schema(ctx)
	if err != nil {
		return err
	}
	got, err := readLegacySchema(ctx, querier)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		mismatch := "object count"
		for index := 0; index < min(len(got), len(want)); index++ {
			if got[index] != want[index] {
				mismatch = fmt.Sprintf("object %s %q", got[index].kind, got[index].name)
				break
			}
		}
		return fmt.Errorf("%w: database is not the exact predecessor v1 schema (%s; objects=%d, expected=%d)",
			ErrSchemaIncompatible, mismatch, len(got), len(want))
	}
	return nil
}

func canonicalLegacyV1Schema(ctx context.Context) ([]legacySchemaObject, error) {
	if len(migrations) == 0 || migrations[0].version != 1 || migrations[0].sql == "" || migrations[0].apply != nil {
		return nil, fmt.Errorf("%w: binary lacks one canonical predecessor schema", ErrSchemaIncompatible)
	}
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("%w: open canonical predecessor schema: %w", ErrSchemaIncompatible, err)
	}
	database.SetMaxOpenConns(1)
	defer func() { _ = database.Close() }()
	if _, err := database.ExecContext(ctx, createMigrationLedger); err != nil {
		return nil, fmt.Errorf("%w: create canonical predecessor migration ledger: %w", ErrSchemaIncompatible, err)
	}
	if _, err := database.ExecContext(ctx, migrations[0].sql); err != nil {
		return nil, fmt.Errorf("%w: create canonical predecessor schema: %w", ErrSchemaIncompatible, err)
	}
	return readLegacySchema(ctx, database)
}

func readLegacySchema(ctx context.Context, querier legacySchemaQuerier) ([]legacySchemaObject, error) {
	rows, err := querier.QueryContext(ctx, `
SELECT type, name, tbl_name, COALESCE(sql, '')
FROM sqlite_schema
ORDER BY type, name, tbl_name`)
	if err != nil {
		return nil, fmt.Errorf("%w: read predecessor schema: %w", ErrSchemaIncompatible, err)
	}
	defer func() { _ = rows.Close() }()
	var objects []legacySchemaObject
	for rows.Next() {
		var object legacySchemaObject
		var definition string
		if err := rows.Scan(&object.kind, &object.name, &object.table, &definition); err != nil {
			return nil, fmt.Errorf("%w: scan predecessor schema: %w", ErrSchemaIncompatible, err)
		}
		object.definition, err = normalizeSchemaSQL(definition)
		if err != nil {
			return nil, fmt.Errorf("%w: normalize predecessor object %s: %w", ErrSchemaIncompatible, object.name, err)
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate predecessor schema: %w", ErrSchemaIncompatible, err)
	}
	return objects, nil
}

// normalizeSchemaSQL removes formatting-only differences without making
// identifiers collide or altering quoted values. sqlite_schema already strips
// IF NOT EXISTS and database qualifiers; token boundaries preserve the rest of
// the canonical definition exactly.
func normalizeSchemaSQL(statement string) (string, error) {
	var tokens []string
	for offset := 0; offset < len(statement); {
		r, width := rune(statement[offset]), 1
		if r >= utf8.RuneSelf {
			r, width = utf8.DecodeRuneInString(statement[offset:])
		}
		if unicode.IsSpace(r) {
			offset += width
			continue
		}
		if isSchemaWordByte(statement[offset]) {
			start := offset
			for offset < len(statement) && isSchemaWordByte(statement[offset]) {
				offset++
			}
			tokens = append(tokens, strings.ToLower(statement[start:offset]))
			continue
		}
		if statement[offset] == '\'' || statement[offset] == '"' || statement[offset] == '`' || statement[offset] == '[' {
			start := offset
			open := statement[offset]
			closeQuote := open
			if open == '[' {
				closeQuote = ']'
			}
			offset++
			closed := false
			for offset < len(statement) {
				if statement[offset] != closeQuote {
					offset++
					continue
				}
				offset++
				if open != '[' && offset < len(statement) && statement[offset] == closeQuote {
					offset++
					continue
				}
				closed = true
				break
			}
			if !closed {
				return "", fmt.Errorf("unterminated quoted token at byte %d", start)
			}
			token := statement[start:offset]
			if open != '\'' {
				if identifier, ok := normalizeQuotedSchemaIdentifier(token, open); ok {
					token = identifier
				}
			}
			tokens = append(tokens, token)
			continue
		}
		tokens = append(tokens, statement[offset:offset+width])
		offset += width
	}
	return strings.Join(tokens, "\x00"), nil
}

func isSchemaWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_'
}

func normalizeQuotedSchemaIdentifier(token string, open byte) (string, bool) {
	if len(token) < 2 {
		return "", false
	}
	identifier := token[1 : len(token)-1]
	switch open {
	case '"':
		identifier = strings.ReplaceAll(identifier, `""`, `"`)
	case '`':
		identifier = strings.ReplaceAll(identifier, "``", "`")
	case '[':
		identifier = strings.ReplaceAll(identifier, "]]", "]")
	default:
		return "", false
	}
	for index := range len(identifier) {
		value := identifier[index]
		if index == 0 && value >= '0' && value <= '9' || !isSchemaWordByte(value) {
			return "", false
		}
	}
	if identifier == "" {
		return "", false
	}
	return strings.ToLower(identifier), true
}
