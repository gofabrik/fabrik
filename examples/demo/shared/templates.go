package shared

import (
	"fmt"
	"strings"
	"time"
)

//fabrik:templates:func
func Shout(s string) string { return strings.ToUpper(s) }

//fabrik:templates:func
func HumanizeAge(t time.Time) string {
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
