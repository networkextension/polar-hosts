package hosts

// skill_catalog_store.go — typed CRUD for skill_catalog rows.
//
// One row per (publisher, skill_kind, version). Soft delete via retired_at.
// Schema lives in scripts/migrate/skill-catalog.sql.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type SkillCatalogEntry struct {
	ID              string          `json:"id"`
	Publisher       string          `json:"publisher"`
	SkillKind       string          `json:"skill_kind"`
	Version         string          `json:"version"`
	SHA256          string          `json:"sha256"`
	DownloadURL     string          `json:"download_url"`
	SizeBytes       int64           `json:"size_bytes"`
	ManifestJSON    json.RawMessage `json:"manifest_json"`
	DisplayName     string          `json:"display_name"`
	Description     string          `json:"description"`
	License         string          `json:"license"`
	Homepage        string          `json:"homepage"`
	PublisherPubkey string          `json:"publisher_pubkey,omitempty"`
	SignedAt        *time.Time      `json:"signed_at,omitempty"`
	IsPlatform      bool            `json:"is_platform"`
	WorkspaceID     string          `json:"workspace_id,omitempty"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       time.Time       `json:"created_at"`
	RetiredAt       *time.Time      `json:"retired_at,omitempty"`
}

// CreateSkillCatalogEntry inserts a new catalog row. Returns the
// inserted entry with id+created_at populated. Returns ErrConflict
// on duplicate (publisher, skill_kind, version).
func (p *Plugin) CreateSkillCatalogEntry(in *SkillCatalogEntry) (*SkillCatalogEntry, error) {
	if err := validateSkillCatalogEntry(in); err != nil {
		return nil, err
	}
	in.ID = "scat_" + generateResourceID()
	in.CreatedAt = time.Now().UTC()
	if in.ManifestJSON == nil {
		in.ManifestJSON = json.RawMessage("{}")
	}
	_, err := p.DB.Exec(
		`INSERT INTO skill_catalog
		  (id, publisher, skill_kind, version, sha256, download_url, size_bytes,
		   manifest_json, display_name, description, license, homepage,
		   publisher_pubkey, signed_at, is_platform, workspace_id, created_by, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		in.ID, in.Publisher, in.SkillKind, in.Version, in.SHA256, in.DownloadURL,
		in.SizeBytes, []byte(in.ManifestJSON), in.DisplayName, in.Description,
		in.License, in.Homepage, in.PublisherPubkey, in.SignedAt,
		in.IsPlatform, in.WorkspaceID, in.CreatedBy, in.CreatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSkillCatalogConflict
		}
		return nil, err
	}
	return in, nil
}

func (p *Plugin) GetSkillCatalogEntry(id string) (*SkillCatalogEntry, error) {
	row := p.DB.QueryRow(
		`SELECT id, publisher, skill_kind, version, sha256, download_url, size_bytes,
		        manifest_json, display_name, description, license, homepage,
		        publisher_pubkey, signed_at, is_platform, workspace_id, created_by, created_at, retired_at
		   FROM skill_catalog WHERE id = $1`, id,
	)
	return scanSkillCatalogEntry(row)
}

// ListSkillCatalog returns catalog entries visible to the requesting
// workspace:
//   - is_platform = TRUE (any workspace)
//   - workspace_id = <wid>
// Excludes retired entries unless includeRetired.
func (p *Plugin) ListSkillCatalog(workspaceID string, includeRetired bool) ([]SkillCatalogEntry, error) {
	q := `SELECT id, publisher, skill_kind, version, sha256, download_url, size_bytes,
	             manifest_json, display_name, description, license, homepage,
	             publisher_pubkey, signed_at, is_platform, workspace_id, created_by, created_at, retired_at
	        FROM skill_catalog
	       WHERE (is_platform = TRUE OR workspace_id = $1)`
	if !includeRetired {
		q += ` AND retired_at IS NULL`
	}
	q += ` ORDER BY publisher ASC, skill_kind ASC, version DESC`
	rows, err := p.DB.Query(q, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SkillCatalogEntry, 0)
	for rows.Next() {
		e, err := scanSkillCatalogEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// RetireSkillCatalogEntry soft-deletes by setting retired_at = now().
// Idempotent: re-retiring is fine.
func (p *Plugin) RetireSkillCatalogEntry(id string) error {
	res, err := p.DB.Exec(`UPDATE skill_catalog SET retired_at = now() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrSkillCatalogNotFound
	}
	return nil
}

// FindSkillCatalogByTriple looks up a non-retired entry by
// (publisher, skill_kind, version). Used by the install endpoint
// to resolve "install kdp 0.3.0" without forcing the client to
// pass the catalog ID.
func (p *Plugin) FindSkillCatalogByTriple(publisher, kind, version string) (*SkillCatalogEntry, error) {
	row := p.DB.QueryRow(
		`SELECT id, publisher, skill_kind, version, sha256, download_url, size_bytes,
		        manifest_json, display_name, description, license, homepage,
		        publisher_pubkey, signed_at, is_platform, workspace_id, created_by, created_at, retired_at
		   FROM skill_catalog
		  WHERE publisher = $1 AND skill_kind = $2 AND version = $3 AND retired_at IS NULL
		  LIMIT 1`,
		publisher, kind, version,
	)
	return scanSkillCatalogEntry(row)
}

// --- helpers ---

var (
	ErrSkillCatalogNotFound = errors.New("skill catalog entry not found")
	ErrSkillCatalogConflict = errors.New("skill catalog entry already exists for (publisher, kind, version)")
)

func scanSkillCatalogEntry(r rowScanner) (*SkillCatalogEntry, error) {
	var e SkillCatalogEntry
	var manifest []byte
	var signedAt, retiredAt sql.NullTime
	err := r.Scan(
		&e.ID, &e.Publisher, &e.SkillKind, &e.Version, &e.SHA256, &e.DownloadURL,
		&e.SizeBytes, &manifest, &e.DisplayName, &e.Description, &e.License, &e.Homepage,
		&e.PublisherPubkey, &signedAt, &e.IsPlatform, &e.WorkspaceID,
		&e.CreatedBy, &e.CreatedAt, &retiredAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSkillCatalogNotFound
		}
		return nil, err
	}
	e.ManifestJSON = json.RawMessage(manifest)
	if signedAt.Valid {
		t := signedAt.Time.UTC()
		e.SignedAt = &t
	}
	if retiredAt.Valid {
		t := retiredAt.Time.UTC()
		e.RetiredAt = &t
	}
	return &e, nil
}

func validateSkillCatalogEntry(e *SkillCatalogEntry) error {
	if e == nil {
		return errors.New("entry required")
	}
	if strings.TrimSpace(e.Publisher) == "" {
		return errors.New("publisher required")
	}
	if strings.TrimSpace(e.SkillKind) == "" {
		return errors.New("skill_kind required")
	}
	if strings.TrimSpace(e.Version) == "" {
		return errors.New("version required")
	}
	if strings.TrimSpace(e.SHA256) == "" {
		return errors.New("sha256 required")
	}
	if len(e.SHA256) != 64 {
		return fmt.Errorf("sha256: expected 64 hex chars, got %d", len(e.SHA256))
	}
	if strings.TrimSpace(e.DownloadURL) == "" {
		return errors.New("download_url required")
	}
	if e.SizeBytes <= 0 {
		return errors.New("size_bytes must be > 0")
	}
	if strings.TrimSpace(e.CreatedBy) == "" {
		return errors.New("created_by required")
	}
	return nil
}

func isUniqueViolation(err error) bool {
	// pq error code 23505 = unique_violation. Avoid pulling the pq
	// types dependency just for one constant; string-match is fine
	// here and matches the convention used elsewhere in the repo.
	return err != nil && strings.Contains(err.Error(), "duplicate key value violates unique constraint")
}
