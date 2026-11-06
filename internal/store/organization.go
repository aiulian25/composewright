package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ErrOrganizationInvalid = errors.New("invalid project organization")

var ErrOrganizationStale = errors.New("project organization changed; reload Projects before trying again")

// NormalizeProjectLabels bounds non-secret, local organization names. Case
// differences identify one label; first spelling wins and ordering is stable.
func NormalizeProjectLabels(labels []string) ([]string, error) {
	if len(labels) > 8 {
		return nil, errors.New("use at most eight labels per project")
	}
	normalized := []string{}
	seen := map[string]bool{}
	for _, label := range labels {
		if !utf8.ValidString(label) || strings.ContainsFunc(label, unicode.IsControl) {
			return nil, errors.New("labels cannot contain control characters")
		}
		label = strings.Join(strings.Fields(label), " ")
		if label == "" {
			continue
		}
		if utf8.RuneCountInString(label) > 32 {
			return nil, errors.New("each label must be at most 32 characters")
		}
		for _, character := range label {
			if !unicode.IsLetter(character) && !unicode.IsNumber(character) && character != ' ' && character != '-' && character != '_' {
				return nil, errors.New("labels use letters, numbers, spaces, - or _")
			}
		}
		key := strings.ToLower(label)
		if !seen[key] {
			seen[key] = true
			normalized = append(normalized, label)
		}
	}
	slices.SortFunc(normalized, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return normalized, nil
}

func ValidateProjectOrganization(project Project) error {
	labels, err := NormalizeProjectLabels(project.Labels)
	if err != nil {
		return err
	}
	if !slices.Equal(labels, project.Labels) || project.PinOrder < 0 || project.PinOrder > 1000000 {
		return errors.New("invalid project labels or pin order")
	}
	return nil
}

func readOrganization(ctx context.Context, query rowQuerier, project *Project) error {
	var labels string
	err := query.QueryRowContext(ctx, `SELECT labels_json,pin_order,revision FROM project_organization WHERE project_id=?`, project.ID).Scan(&labels, &project.PinOrder, &project.OrganizationRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(labels), &project.Labels)
}

// OrganizeProject commits labels and the complete pin ordering together. It
// touches no deployment fields or timestamps. Every changed position advances
// its revision so stale forms cannot undo another tab's reorder.
func (store *Store) OrganizeProject(ctx context.Context, id string, revision int64, labels []string, action string) error {
	normalized, err := NormalizeProjectLabels(labels)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOrganizationInvalid, err)
	}
	if action != "save" && action != "pin" && action != "unpin" && action != "up" && action != "down" {
		return fmt.Errorf("%w: unknown action", ErrOrganizationInvalid)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return err
	}
	return store.transaction(ctx, func(tx *sql.Tx) error {
		project, err := scanProject(tx.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id=?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err = readOrganization(ctx, tx, &project); err != nil {
			return err
		}
		if project.OrganizationRevision != revision {
			return ErrOrganizationStale
		}
		rows, err := tx.QueryContext(ctx, `SELECT o.project_id FROM project_organization o JOIN projects p ON p.id=o.project_id WHERE o.pin_order>0 ORDER BY o.pin_order,p.name`)
		if err != nil {
			return err
		}
		var pins []string
		for rows.Next() {
			var pin string
			if err = rows.Scan(&pin); err != nil {
				break
			}
			pins = append(pins, pin)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		index := slices.Index(pins, id)
		switch action {
		case "pin":
			if index < 0 {
				pins = append(pins, id)
			}
		case "unpin":
			if index >= 0 {
				pins = slices.Delete(pins, index, index+1)
			}
		case "up", "down":
			if index < 0 {
				return fmt.Errorf("%w: pin the project before moving it", ErrOrganizationInvalid)
			}
			next := index - 1
			if action == "down" {
				next = index + 1
			}
			if next >= 0 && next < len(pins) {
				pins[index], pins[next] = pins[next], pins[index]
			}
		}
		rank := project.PinOrder
		if action == "unpin" {
			rank = 0
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO project_organization(project_id,labels_json,pin_order,revision) VALUES(?,?,?,1)
   ON CONFLICT(project_id) DO UPDATE SET labels_json=excluded.labels_json,pin_order=excluded.pin_order,revision=project_organization.revision+1`, id, string(encoded), rank)
		if err != nil {
			return err
		}
		for index, pin := range pins {
			if _, err = tx.ExecContext(ctx, `UPDATE project_organization SET pin_order=?,revision=revision+1 WHERE project_id=? AND pin_order<>?`, index+1, pin, index+1); err != nil {
				return err
			}
		}
		return nil
	})
}

func restoreOrganization(ctx context.Context, tx *sql.Tx, project Project) error {
	if err := ValidateProjectOrganization(project); err != nil {
		return fmt.Errorf("project organization: %w", err)
	}
	if len(project.Labels) == 0 && project.PinOrder == 0 {
		return nil
	}
	labels, err := json.Marshal(project.Labels)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_organization(project_id,labels_json,pin_order,revision) VALUES(?,?,?,1)`, project.ID, string(labels), project.PinOrder)
	return err
}
