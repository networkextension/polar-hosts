package hosts

// console_layouts store helpers — per-user named tile layouts for
// /console.html. Each layout is scoped to (workspace_id, owner_user_id,
// name). panes_config is a JSONB array; the row's lifetime is bound to
// both the workspace and the user (ON DELETE CASCADE on both FKs).
//
// Loading a layout doesn't materialize anything server-side — the UI
// just reads panes_config + calls POST /shell/open for each entry. So
// these helpers are a thin CRUD wrapper around the table.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const consoleLayoutMaxPanes = 9

// ConsolePaneConfig is one entry in panes_config. Position is by
// array index in the parent ConsoleLayout.PanesConfig.
type ConsolePaneConfig struct {
	HostID      string `json:"host_id"`
	HostSkillID int64  `json:"host_skill_id"`
}

// ConsoleLayout mirrors the row + decoded panes_config.
type ConsoleLayout struct {
	ID          int64               `json:"id"`
	WorkspaceID string              `json:"workspace_id"`
	OwnerUserID string              `json:"owner_user_id"`
	Name        string              `json:"name"`
	PanesConfig []ConsolePaneConfig `json:"panes_config"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

// validateConsolePanesConfig is the server-side guard. UI shouldn't
// produce out-of-shape values, but a malicious or buggy client could.
// We enforce: 1..9 entries, host_id 1..64 chars, host_skill_id > 0.
func validateConsolePanesConfig(panes []ConsolePaneConfig) error {
	if len(panes) == 0 {
		return errors.New("panes_config must have at least 1 entry")
	}
	if len(panes) > consoleLayoutMaxPanes {
		return fmt.Errorf("panes_config has %d entries; max is %d", len(panes), consoleLayoutMaxPanes)
	}
	for i, p := range panes {
		hostID := strings.TrimSpace(p.HostID)
		if hostID == "" || len(hostID) > 64 {
			return fmt.Errorf("panes_config[%d].host_id must be 1..64 chars", i)
		}
		if p.HostSkillID <= 0 {
			return fmt.Errorf("panes_config[%d].host_skill_id must be positive", i)
		}
	}
	return nil
}

func (p *Plugin) createConsoleLayout(workspaceID, ownerUserID, name string, panes []ConsolePaneConfig, now time.Time) (*ConsoleLayout, error) {
	if strings.TrimSpace(workspaceID) == "" {
		return nil, errors.New("workspace_id required")
	}
	if strings.TrimSpace(ownerUserID) == "" {
		return nil, errors.New("owner_user_id required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name required")
	}
	if len(name) > 80 {
		return nil, errors.New("name too long (max 80 chars)")
	}
	if err := validateConsolePanesConfig(panes); err != nil {
		return nil, err
	}
	panesJSON, err := json.Marshal(panes)
	if err != nil {
		return nil, fmt.Errorf("marshal panes_config: %w", err)
	}

	var id int64
	err = p.DB.QueryRow(`
		INSERT INTO console_layouts (workspace_id, owner_user_id, name, panes_config, created_at, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $5)
		RETURNING id`,
		workspaceID, ownerUserID, name, string(panesJSON), now,
	).Scan(&id)
	if err != nil {
		return nil, err
	}
	return &ConsoleLayout{
		ID:          id,
		WorkspaceID: workspaceID,
		OwnerUserID: ownerUserID,
		Name:        name,
		PanesConfig: panes,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (p *Plugin) listConsoleLayoutsForOwner(workspaceID, ownerUserID string) ([]ConsoleLayout, error) {
	rows, err := p.DB.Query(`
		SELECT id, workspace_id, owner_user_id, name, panes_config::text, created_at, updated_at
		FROM console_layouts
		WHERE workspace_id = $1 AND owner_user_id = $2
		ORDER BY updated_at DESC`,
		workspaceID, ownerUserID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConsoleLayout
	for rows.Next() {
		var (
			l         ConsoleLayout
			panesText string
		)
		if err := rows.Scan(&l.ID, &l.WorkspaceID, &l.OwnerUserID, &l.Name, &panesText, &l.CreatedAt, &l.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(panesText), &l.PanesConfig); err != nil {
			// Tolerate corrupt rows by returning them with empty
			// panes — the UI shows "empty layout" rather than failing
			// the whole list.
			l.PanesConfig = nil
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (p *Plugin) getConsoleLayout(workspaceID, ownerUserID string, id int64) (*ConsoleLayout, error) {
	var (
		l         ConsoleLayout
		panesText string
	)
	err := p.DB.QueryRow(`
		SELECT id, workspace_id, owner_user_id, name, panes_config::text, created_at, updated_at
		FROM console_layouts
		WHERE id = $1 AND workspace_id = $2 AND owner_user_id = $3`,
		id, workspaceID, ownerUserID,
	).Scan(&l.ID, &l.WorkspaceID, &l.OwnerUserID, &l.Name, &panesText, &l.CreatedAt, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(panesText), &l.PanesConfig); err != nil {
		l.PanesConfig = nil
	}
	return &l, nil
}

// updateConsoleLayout supports partial updates: pass nil pointers to
// leave a field unchanged. Owner-scoped — the WHERE prevents one user
// from editing another's layout even if they guess the id.
func (p *Plugin) updateConsoleLayout(workspaceID, ownerUserID string, id int64, name *string, panes *[]ConsolePaneConfig, now time.Time) (*ConsoleLayout, error) {
	if name == nil && panes == nil {
		return p.getConsoleLayout(workspaceID, ownerUserID, id)
	}
	// Build SET clause dynamically. Trivially small; no need for a
	// query builder.
	sets := []string{"updated_at = $1"}
	args := []any{now}
	idx := 2
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		if trimmed == "" {
			return nil, errors.New("name cannot be empty")
		}
		if len(trimmed) > 80 {
			return nil, errors.New("name too long (max 80 chars)")
		}
		sets = append(sets, fmt.Sprintf("name = $%d", idx))
		args = append(args, trimmed)
		idx++
	}
	if panes != nil {
		if err := validateConsolePanesConfig(*panes); err != nil {
			return nil, err
		}
		panesJSON, err := json.Marshal(*panes)
		if err != nil {
			return nil, fmt.Errorf("marshal panes_config: %w", err)
		}
		sets = append(sets, fmt.Sprintf("panes_config = $%d::jsonb", idx))
		args = append(args, string(panesJSON))
		idx++
	}
	// WHERE clause params follow the SET values.
	whereStart := idx
	args = append(args, id, workspaceID, ownerUserID)

	q := fmt.Sprintf(`UPDATE console_layouts SET %s WHERE id = $%d AND workspace_id = $%d AND owner_user_id = $%d`,
		strings.Join(sets, ", "), whereStart, whereStart+1, whereStart+2)
	res, err := p.DB.Exec(q, args...)
	if err != nil {
		return nil, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return nil, nil
	}
	return p.getConsoleLayout(workspaceID, ownerUserID, id)
}

func (p *Plugin) deleteConsoleLayout(workspaceID, ownerUserID string, id int64) (bool, error) {
	res, err := p.DB.Exec(`
		DELETE FROM console_layouts
		WHERE id = $1 AND workspace_id = $2 AND owner_user_id = $3`,
		id, workspaceID, ownerUserID,
	)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}
