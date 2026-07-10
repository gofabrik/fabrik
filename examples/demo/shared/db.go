package shared

import (
	"database/sql"

	"github.com/gofabrik/fabrik/migrations"
	_ "modernc.org/sqlite"
)

//fabrik:config database
type Database struct {
	Path string `yaml:"path" env:"DEMO_DATABASE_PATH" default:"demo.db"`
}

//fabrik:provider
func NewDB(cfg *Database) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+cfg.Path)
}

//fabrik:provider
func NewDialect() migrations.Dialect { return migrations.DialectSQLite }
