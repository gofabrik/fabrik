package session

import (
	"errors"
	"time"
)

// defaultMaxRetries bounds CAS and SID-mint conflict loops.
const defaultMaxRetries = 3

// Config configures a [Manager]. Store and Token are required;
// everything else has a working zero value.
type Config struct {
	Store Store // required
	Token Token // required: Cookie, Bearer, or Multi

	AbsoluteExpiry time.Duration // required, > 0
	IdleExpiry     time.Duration // 0 disables idle expiry

	// 0 disables read-path bumps; must not exceed IdleExpiry.
	IdleBumpInterval time.Duration

	// 0 means the package default; negative disables retries.
	MaxRetries int

	Now func() time.Time // nil means time.Now

	// nil means the built-in generator. Empty SIDs fail without retry.
	NewSID func() (string, error)
}

// validate collects every configuration problem in one pass.
func (cfg Config) validate() error {
	var problems []error
	if cfg.Store == nil {
		problems = append(problems, errors.New("session.New: Store is required"))
	}
	if cfg.Token == nil {
		problems = append(problems, errors.New("session.New: Token is required"))
	} else if v, ok := cfg.Token.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			problems = append(problems, err)
		}
	}
	if cfg.AbsoluteExpiry <= 0 {
		problems = append(problems, errors.New("session.New: AbsoluteExpiry must be > 0"))
	}
	if cfg.IdleExpiry < 0 {
		problems = append(problems, errors.New("session.New: IdleExpiry must not be negative"))
	}
	if cfg.IdleExpiry > 0 && cfg.AbsoluteExpiry > 0 && cfg.IdleExpiry > cfg.AbsoluteExpiry {
		problems = append(problems, errors.New("session.New: IdleExpiry must not exceed AbsoluteExpiry"))
	}
	if cfg.IdleBumpInterval < 0 {
		problems = append(problems, errors.New("session.New: IdleBumpInterval must not be negative"))
	}
	if cfg.IdleBumpInterval > cfg.IdleExpiry {
		problems = append(problems, errors.New("session.New: IdleBumpInterval must not exceed IdleExpiry (0 when idle expiry is disabled)"))
	}
	return errors.Join(problems...)
}
