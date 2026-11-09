package store

import (
	"context"
)

// ProjectActivity summarises one project for the overview's project cards.
type ProjectActivity struct {
	ProjectID  int64
	Events     int64   // events in the current window
	Previous   int64   // events in the window before it, for the change badge
	Unresolved int64   // unresolved issues, all time
	Days       []int64 // events per day in the current window, oldest first
}

// Change is the difference between this window and the previous one.
func (p ProjectActivity) Change() int64 { return p.Events - p.Previous }

// ProjectsActivity returns per-project activity for the last `days` days
// (and the `days` before that), read from the daily_totals rollup.
func (s *Store) ProjectsActivity(ctx context.Context, days int) (map[int64]*ProjectActivity, error) {
	today := s.now().UTC().UnixMilli() / dayMs
	first := today - int64(days) + 1
	prevFirst := first - int64(days)

	out := map[int64]*ProjectActivity{}
	get := func(id int64) *ProjectActivity {
		p, ok := out[id]
		if !ok {
			p = &ProjectActivity{ProjectID: id, Days: make([]int64, days)}
			out[id] = p
		}
		return p
	}

	rows, err := s.reader.QueryContext(ctx,
		`SELECT project_id, day, errors + queue_failures FROM daily_totals WHERE day >= ?`, prevFirst)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, day, n int64
		if err := rows.Scan(&id, &day, &n); err != nil {
			return nil, err
		}
		p := get(id)
		if day >= first {
			p.Events += n
			if i := day - first; i < int64(days) {
				p.Days[i] = n
			}
		} else {
			p.Previous += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	unresolved, err := s.reader.QueryContext(ctx,
		`SELECT project_id, COUNT(*) FROM issues WHERE status = 'unresolved' GROUP BY project_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unresolved.Close() }()
	for unresolved.Next() {
		var id, n int64
		if err := unresolved.Scan(&id, &n); err != nil {
			return nil, err
		}
		get(id).Unresolved = n
	}
	return out, unresolved.Err()
}
