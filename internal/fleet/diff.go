package fleet

import "reflect"

// Changes lists Computers by what applying a new Fleet does to them. Each list
// is sorted. Drives are never removed by applying a Fleet.
type Changes struct {
	Created   []string `json:"created"`
	Updated   []string `json:"updated"`
	Removed   []string `json:"removed"`
	Unchanged []string `json:"unchanged"`
}

// Diff compares two resolved Fleets. old may be nil (nothing applied yet).
func Diff(old, new *Fleet) Changes {
	var ch Changes
	var before map[string]Computer
	if old != nil {
		before = old.Computers
	}
	for _, name := range new.ComputerNames() {
		prev, ok := before[name]
		switch {
		case !ok:
			ch.Created = append(ch.Created, name)
		case !reflect.DeepEqual(normalize(prev), normalize(new.Computers[name])):
			ch.Updated = append(ch.Updated, name)
		default:
			ch.Unchanged = append(ch.Unchanged, name)
		}
	}
	for _, name := range sortedKeys(before) {
		if _, ok := new.Computers[name]; !ok {
			ch.Removed = append(ch.Removed, name)
		}
	}
	return ch
}

// normalize makes nil and empty lists compare equal.
func normalize(c Computer) Computer {
	if len(c.Credentials) == 0 {
		c.Credentials = nil
	}
	if len(c.Drives) == 0 {
		c.Drives = nil
	}
	if len(c.Egress.Allow) == 0 {
		c.Egress.Allow = nil
	}
	return c
}
