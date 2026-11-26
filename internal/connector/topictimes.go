package connector

import (
	"encoding/json"
	"fmt"
	"time"
)

// TopicTimes maps a subject to the newest activity on it. It is stored as Unix
// seconds: source timestamps carry no finer precision worth keeping, and
// written as text the dates doubled the size of the stored records.
type TopicTimes map[string]time.Time

// MarshalJSON writes each date as Unix seconds.
func (tt TopicTimes) MarshalJSON() ([]byte, error) {
	secs := make(map[string]int64, len(tt))
	for top, at := range tt {
		secs[top] = at.Unix()
	}
	return json.Marshal(secs)
}

// UnmarshalJSON reads dates written as Unix seconds, in UTC.
func (tt *TopicTimes) UnmarshalJSON(raw []byte) error {
	var secs map[string]int64
	if err := json.Unmarshal(raw, &secs); err != nil {
		return fmt.Errorf("%w: %w", ErrTopicTimes, err)
	}
	if secs == nil {
		*tt = nil
		return nil
	}
	out := make(TopicTimes, len(secs))
	for top, s := range secs {
		out[top] = time.Unix(s, 0).UTC()
	}
	*tt = out
	return nil
}

// noteTopicTime moves the date of each token key worked on forward to at, so
// every subject carries the newest activity on it rather than the newest
// activity of the person anywhere.
func noteTopicTime(into map[string]map[string]time.Time, key string, toks []string, at time.Time) {
	if at.IsZero() || len(toks) == 0 {
		return
	}
	m := into[key]
	if m == nil {
		m = make(map[string]time.Time, len(toks))
		into[key] = m
	}
	for _, tok := range toks {
		if tok != "" && at.After(m[tok]) {
			m[tok] = at
		}
	}
}

// mergeTopicTimes folds from into into, keeping the later date for every
// subject both hold.
func mergeTopicTimes(into, from map[string]map[string]time.Time) {
	for key, dates := range from {
		m := into[key]
		if m == nil {
			m = make(map[string]time.Time, len(dates))
			into[key] = m
		}
		for tok, at := range dates {
			if at.After(m[tok]) {
				m[tok] = at
			}
		}
	}
}
