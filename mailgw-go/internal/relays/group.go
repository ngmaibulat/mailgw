package relays

import "math/rand/v2"

// Attempts returns the ordered dial list for one delivery attempt.
//
// Members are already sorted by priority. Within a priority band the order is
// randomised so that equal-priority relays share load, while lower-priority
// bands are only reached after every member of a higher band has failed.
func (g *Group) Attempts(rnd *rand.Rand) []Relay {
	if g == nil || len(g.Members) == 0 {
		return nil
	}

	out := append([]Relay(nil), g.Members...)
	shuffle := rand.Shuffle
	if rnd != nil {
		shuffle = rnd.Shuffle
	}

	// Walk each run of equal priority and shuffle it in place.
	for start := 0; start < len(out); {
		end := start + 1
		for end < len(out) && out[end].Priority == out[start].Priority {
			end++
		}
		if band := out[start:end]; len(band) > 1 {
			shuffle(len(band), func(i, j int) { band[i], band[j] = band[j], band[i] })
		}
		start = end
	}
	return out
}
