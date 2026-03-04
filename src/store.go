package src

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// DefaultPageSize is the number of sentences shown per query page.
const DefaultPageSize = 10

var ErrNotFound = errors.New("record not found")

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	id          INTEGER  PRIMARY KEY AUTOINCREMENT,
	author_id   TEXT     NOT NULL,
	author_name TEXT     NOT NULL,
	channel_id  TEXT     NOT NULL,
	sentence    TEXT     NOT NULL,
	explanation TEXT     NOT NULL,
	created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_messages_author  ON messages(author_id);
CREATE INDEX IF NOT EXISTS idx_messages_created ON messages(created_at DESC);
`

// Record is a single row in the messages table.
type Record struct {
	ID          int64
	AuthorID    string
	AuthorName  string
	ChannelID   string
	Sentence    string
	Explanation string
	CreatedAt   time.Time
}

// Page is the result of a paginated Query call.
type Page struct {
	Records     []Record
	CurrentPage int
	PageSize    int
	TotalCount  int
	TotalPages  int
	HasPrev     bool
	HasNext     bool
}

type Store struct {
	db *sql.DB
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite at %q: %w", path, err)
	}

	// One writer at a time prevents "database is locked" errors.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run schema migrations: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Save(ctx context.Context, r Record) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (author_id, author_name, channel_id, sentence, explanation, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.AuthorID, r.AuthorName, r.ChannelID,
		r.Sentence, r.Explanation,
		time.Now().UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("insert record: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) Query(ctx context.Context, search string, page, pageSize int) (*Page, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = DefaultPageSize
	}

	pattern := "%" + search + "%"
	offset := (page - 1) * pageSize

	var total int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM   messages
		WHERE  sentence    LIKE ? COLLATE NOCASE
		   OR  explanation LIKE ? COLLATE NOCASE
		   OR  author_name LIKE ? COLLATE NOCASE`,
		pattern, pattern, pattern,
	).Scan(&total)
	if err != nil {
		return nil, fmt.Errorf("count query: %w", err)
	}

	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, author_id, author_name, channel_id, sentence, explanation, created_at
		FROM   messages
		WHERE  sentence    LIKE ? COLLATE NOCASE
		   OR  explanation LIKE ? COLLATE NOCASE
		   OR  author_name LIKE ? COLLATE NOCASE
		ORDER  BY created_at DESC
		LIMIT  ? OFFSET ?`,
		pattern, pattern, pattern, pageSize, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("page query: %w", err)
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(
			&r.ID, &r.AuthorID, &r.AuthorName, &r.ChannelID,
			&r.Sentence, &r.Explanation, &r.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}

	return &Page{
		Records:     records,
		CurrentPage: page,
		PageSize:    pageSize,
		TotalCount:  total,
		TotalPages:  totalPages,
		HasPrev:     page > 1,
		HasNext:     page < totalPages,
	}, nil
}

func (s *Store) GetByID(ctx context.Context, id int64) (*Record, error) {
	var r Record
	err := s.db.QueryRowContext(ctx, `
		SELECT id, author_id, author_name, channel_id, sentence, explanation, created_at
		FROM   messages
		WHERE  id = ?`, id,
	).Scan(&r.ID, &r.AuthorID, &r.AuthorName, &r.ChannelID, &r.Sentence, &r.Explanation, &r.CreatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get by id: %w", err)
	}
	return &r, nil
}
