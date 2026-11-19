package app

import (
	"errors"
	"slices"
	"strings"
	"sync"

	"hostbay/internal/database"
	"hostbay/internal/store"
)

// A database tab's history and saved queries live on this computer only,
// by host: the history in store's history file (KTD39), which sync and
// backup never carry, and saved queries in databaseQueriesName.
const (
	databaseQueriesName = "database-queries.json"
	historyQueryMax     = 64 << 10 // a longer request is not kept
)

// SavedQuery is a query saved under a name for its host.
type SavedQuery struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

var savedQueriesMu sync.Mutex

// historyKey is where host's database requests are kept.
func historyKey(hostID string) string { return "database:" + hostID }

// secretLike reports a request that likely carries a credential, such as
// CREATE USER … PASSWORD or IDENTIFIED BY, which history does not keep.
func secretLike(text string) bool {
	u := strings.ToUpper(text)
	return strings.Contains(u, "PASSWORD") || strings.Contains(u, "IDENTIFIED BY") || strings.Contains(u, "SECRET")
}

// remember adds text to d's host's history, unless it repeats the last
// entry, is too long to be worth keeping, or looks like it holds a
// credential; the connection's own password is removed first.
func (s *DatabaseService) remember(d *dbSession, text string) {
	text = strings.TrimSpace(database.Scrub(text, d.secret))
	if s.creds == nil || s.creds.st == nil || d.hostID == "" || text == "" || len(text) > historyQueryMax || secretLike(text) {
		return
	}
	key := historyKey(d.hostID)
	if h, err := s.creds.st.History(key); err == nil && len(h) > 0 && h[len(h)-1] == text {
		return
	}
	_ = s.creds.st.AddHistory(key, text)
}

// DatabaseHistory lists the requests run in this host's tabs on this
// computer, oldest first.
func (s *DatabaseService) DatabaseHistory(dbID string) ([]string, error) {
	d, err := s.session(dbID)
	if err != nil {
		return nil, err
	}
	h, err := s.creds.st.History(historyKey(d.hostID))
	if h == nil {
		h = []string{}
	}
	return h, err
}

// DatabaseClearHistory forgets this host's request history.
func (s *DatabaseService) DatabaseClearHistory(dbID string) error {
	d, err := s.session(dbID)
	if err != nil {
		return err
	}
	return s.creds.st.ClearHistory(historyKey(d.hostID))
}

// editSaved changes the saved queries of dbID's host under the lock.
func (s *DatabaseService) editSaved(dbID string, fn func([]SavedQuery) ([]SavedQuery, error)) ([]SavedQuery, error) {
	d, err := s.session(dbID)
	if err != nil {
		return nil, err
	}
	savedQueriesMu.Lock()
	defer savedQueriesMu.Unlock()
	all, err := store.Load[map[string][]SavedQuery](s.creds.st, databaseQueriesName)
	if err != nil {
		return nil, err
	}
	if all == nil {
		all = map[string][]SavedQuery{}
	}
	mine := all[d.hostID]
	if fn != nil {
		if mine, err = fn(slices.Clone(mine)); err != nil {
			return nil, err
		}
		if len(mine) == 0 {
			delete(all, d.hostID)
		} else {
			all[d.hostID] = mine
		}
		if err := store.Save(s.creds.st, databaseQueriesName, all); err != nil {
			return nil, err
		}
	}
	if mine == nil {
		mine = []SavedQuery{}
	}
	return mine, nil
}

// DatabaseSavedQueries lists this host's saved queries, by name.
func (s *DatabaseService) DatabaseSavedQueries(dbID string) ([]SavedQuery, error) {
	return s.editSaved(dbID, nil)
}

// DatabaseSaveQuery saves text under name for this host, replacing a
// query saved under that name; the connection's password is removed.
func (s *DatabaseService) DatabaseSaveQuery(dbID, name, text string) ([]SavedQuery, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.TrimSpace(text) == "" {
		return nil, errors.New("a saved query needs a name and some text")
	}
	d, err := s.session(dbID)
	if err != nil {
		return nil, err
	}
	text = database.Scrub(text, d.secret)
	return s.editSaved(dbID, func(qs []SavedQuery) ([]SavedQuery, error) {
		qs = slices.DeleteFunc(qs, func(q SavedQuery) bool { return q.Name == name })
		qs = append(qs, SavedQuery{Name: name, Text: text})
		slices.SortFunc(qs, func(a, b SavedQuery) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
		return qs, nil
	})
}

// DatabaseDeleteQuery removes the query saved under name for this host.
func (s *DatabaseService) DatabaseDeleteQuery(dbID, name string) ([]SavedQuery, error) {
	return s.editSaved(dbID, func(qs []SavedQuery) ([]SavedQuery, error) {
		return slices.DeleteFunc(qs, func(q SavedQuery) bool { return q.Name == name }), nil
	})
}
