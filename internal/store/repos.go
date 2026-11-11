package store

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
)

type Repo struct {
	ID             int64
	FullName       string // owner/name
	CloneURL       string // empty = https://github.com/<FullName>.git
	WebhookSecret  string
	Token          string // empty = use the global token
	TimeoutMinutes int    // 0 = use the global default
	CommitStatus   bool
	Enabled        bool
	CreatedAt      int64
	Branches       []string // filled by ListRepos and GetRepo
}

// Owner and Name split FullName. FullName is validated on the way in.
func (r Repo) Owner() string { o, _, _ := strings.Cut(r.FullName, "/"); return o }
func (r Repo) Name() string  { _, n, _ := strings.Cut(r.FullName, "/"); return n }

const repoCols = `id, full_name, clone_url, webhook_secret, token, timeout_minutes, commit_status, enabled, created_at`

func scanRepo(row interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	err := row.Scan(&r.ID, &r.FullName, &r.CloneURL, &r.WebhookSecret, &r.Token,
		&r.TimeoutMinutes, &r.CommitStatus, &r.Enabled, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func (s *Store) CreateRepo(r Repo, branches []string) (int64, error) {
	var id int64
	err := s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO repos (full_name, clone_url, webhook_secret, token,
			timeout_minutes, commit_status, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			r.FullName, r.CloneURL, r.WebhookSecret, r.Token, r.TimeoutMinutes, r.CommitStatus, r.Enabled, now())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return setBranches(tx, id, branches)
	})
	return id, err
}

// UpdateRepo saves the editable fields and replaces the branch allowlist.
func (s *Store) UpdateRepo(r Repo, branches []string) error {
	return s.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE repos SET clone_url = ?, webhook_secret = ?, token = ?,
			timeout_minutes = ?, commit_status = ?, enabled = ? WHERE id = ?`,
			r.CloneURL, r.WebhookSecret, r.Token, r.TimeoutMinutes, r.CommitStatus, r.Enabled, r.ID)
		if err != nil {
			return err
		}
		return setBranches(tx, r.ID, branches)
	})
}

func setBranches(tx *sql.Tx, repoID int64, branches []string) error {
	if _, err := tx.Exec(`DELETE FROM branches WHERE repo_id = ?`, repoID); err != nil {
		return err
	}
	for _, b := range branches {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO branches (repo_id, name) VALUES (?, ?)`, repoID, b); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetWebhookSecret(repoID int64, secret string) error {
	_, err := s.db.Exec(`UPDATE repos SET webhook_secret = ? WHERE id = ?`, secret, repoID)
	return err
}

// DeleteRepo removes a repo with its branches, builds and artifact rows.
func (s *Store) DeleteRepo(id int64) error {
	_, err := s.db.Exec(`DELETE FROM repos WHERE id = ?`, id)
	return err
}

func (s *Store) GetRepo(id int64) (Repo, error) {
	r, err := scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE id = ?`, id))
	if err != nil {
		return r, err
	}
	r.Branches, err = s.Branches(r.ID)
	return r, err
}

func (s *Store) GetRepoByName(fullName string) (Repo, error) {
	r, err := scanRepo(s.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE full_name = ?`, fullName))
	if err != nil {
		return r, err
	}
	r.Branches, err = s.Branches(r.ID)
	return r, err
}

func (s *Store) ListRepos() ([]Repo, error) {
	rows, err := s.db.Query(`SELECT ` + repoCols + ` FROM repos ORDER BY full_name`)
	if err != nil {
		return nil, err
	}
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Branches, err = s.Branches(out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) Branches(repoID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM branches WHERE repo_id = ?`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	sort.Strings(out)
	return out, rows.Err()
}

func (s *Store) BranchAllowed(repoID int64, branch string) bool {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM branches WHERE repo_id = ? AND name = ?`, repoID, branch).Scan(&n)
	return n > 0
}

// ---- build secrets ----

type Secret struct {
	ID        int64
	Name      string
	AllRepos  bool
	Repos     []string // names of the repos allowed to use it, when not AllRepos
	CreatedAt int64
}

func (s *Store) CreateSecret(name, value string, allRepos bool, repoIDs []int64) error {
	return s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`INSERT INTO secrets (name, value, all_repos, created_at) VALUES (?, ?, ?, ?)`,
			name, value, allRepos, now())
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if allRepos {
			return nil
		}
		for _, rid := range repoIDs {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO secret_repos (secret_id, repo_id) VALUES (?, ?)`, id, rid); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteSecret(id int64) error {
	_, err := s.db.Exec(`DELETE FROM secrets WHERE id = ?`, id)
	return err
}

// ListSecrets returns secret metadata. Values are never read back out for display.
func (s *Store) ListSecrets() ([]Secret, error) {
	rows, err := s.db.Query(`SELECT id, name, all_repos, created_at FROM secrets ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	var out []Secret
	for rows.Next() {
		var sc Secret
		if err := rows.Scan(&sc.ID, &sc.Name, &sc.AllRepos, &sc.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].AllRepos {
			continue
		}
		rr, err := s.db.Query(`SELECT r.full_name FROM secret_repos sr JOIN repos r ON r.id = sr.repo_id
			WHERE sr.secret_id = ? ORDER BY r.full_name`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for rr.Next() {
			var n string
			if err := rr.Scan(&n); err != nil {
				rr.Close()
				return nil, err
			}
			out[i].Repos = append(out[i].Repos, n)
		}
		rr.Close()
	}
	return out, nil
}

// SecretsForRepo returns the name -> value map a build of this repo may see.
// A secret scoped to the repo wins over an all-repos secret of the same name.
func (s *Store) SecretsForRepo(repoID int64) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name, value FROM secrets
		WHERE all_repos = 1 OR id IN (SELECT secret_id FROM secret_repos WHERE repo_id = ?)
		ORDER BY all_repos DESC, id`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var n, v string
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, rows.Err()
}
