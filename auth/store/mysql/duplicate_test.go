package mysql

import (
	"errors"
	"testing"
)

// Duplicate-key detection accepts MySQL and MariaDB message formats.
func TestIsDuplicateKey(t *testing.T) {
	mysqlForm := errors.New("Error 1062 (23000): Duplicate entry 'a@example.com' for key 'password_credentials.password_credentials_email'")
	mariaForm := errors.New("Error 1062 (23000): Duplicate entry 'a@example.com' for key 'password_credentials_email'")
	if !isDuplicateKey(mysqlForm) {
		t.Fatal("MySQL form not recognized")
	}
	if !isDuplicateKey(mariaForm) {
		t.Fatal("MariaDB form not recognized")
	}
	if isDuplicateKey(errors.New("Error 1452 (23000): Cannot add or update a child row")) {
		t.Fatal("non-1062 error matched")
	}
	if isDuplicateKey(nil) {
		t.Fatal("nil error matched")
	}
}
