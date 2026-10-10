// Package events reduces an event log to the current state per key.
package events

// Event is one versioned update to a key.
type Event struct {
	Key     string
	Version int
	Value   string
}

// Latest returns, for each distinct key in evs, the event with the highest
// Version. Rules:
//   - events with an empty Key are ignored;
//   - if several events for a key share the highest Version, the one that
//     appears last in evs wins;
//   - the result is ordered by where each key first appears in evs, whichever
//     event ends up winning;
//   - evs is not modified.
//
// It returns nil when there is nothing to report.
func Latest(evs []Event) []Event {
	var out []Event
	for _, e := range evs {
		if e.Key == "" {
			continue
		}
		found := false
		for i := range out {
			if out[i].Key == e.Key {
				if e.Version >= out[i].Version {
					out[i] = e
				}
				found = true
				break
			}
		}
		if !found {
			out = append(out, e)
		}
	}
	return out
}
