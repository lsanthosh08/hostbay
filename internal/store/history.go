package store

import "sync"

// historyName is this computer's command history (KTD39): the last
// historyMax commands per host, in a 0600 file that is not in Data, so
// sync and backup never carry it.
const (
	historyName = "history.json"
	historyMax  = 1000
)

var historyMu sync.Mutex

// History returns key's commands, oldest first.
func (s *Store) History(key string) ([]string, error) {
	historyMu.Lock()
	defer historyMu.Unlock()
	h, err := Load[map[string][]string](s, historyName)
	return h[key], err
}

// AddHistory appends line to key's history, dropping the oldest past
// historyMax.
func (s *Store) AddHistory(key, line string) error {
	return s.editHistory(func(h map[string][]string) {
		l := append(h[key], line)
		h[key] = l[max(0, len(l)-historyMax):]
	})
}

// ClearHistory removes key's history.
func (s *Store) ClearHistory(key string) error {
	return s.editHistory(func(h map[string][]string) { delete(h, key) })
}

func (s *Store) editHistory(fn func(map[string][]string)) error {
	historyMu.Lock()
	defer historyMu.Unlock()
	h, err := Load[map[string][]string](s, historyName)
	if err != nil {
		return err
	}
	if h == nil {
		h = map[string][]string{}
	}
	fn(h)
	return Save(s, historyName, h)
}
