package product

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration that round-trips Go duration strings ("15m",
// "168h") through JSON and YAML.
//
// The standard library marshals time.Duration as an integer nanosecond count,
// which makes a ConfigMap unreadable — `interval: 900000000000` instead of
// `interval: 15m`. Configuration is read by humans far more often than it is
// parsed, so it takes the string form.
type Duration time.Duration

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Or returns d, or fallback when d is zero.
func (d Duration) Or(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON emits the string form.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts a duration string, and a bare number as nanoseconds so
// that a value we emitted can always be read back.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}

	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string such as \"15m\", got %s", string(b))
	}
	*d = Duration(n)
	return nil
}
