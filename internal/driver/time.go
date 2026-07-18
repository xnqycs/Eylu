package driver

import "time"

func durationSeconds(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}
