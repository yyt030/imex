package common

import (
	"database/sql"
	"strings"
)

// ServerType represents the type of database server.
type ServerType int

const (
	ServerMySQL ServerType = iota
	ServerOceanBase
)

// String returns the string representation of the server type.
func (s ServerType) String() string {
	switch s {
	case ServerOceanBase:
		return "OceanBase"
	default:
		return "MySQL"
	}
}

// DetectServerType detects the database server type by querying version_comment.
func DetectServerType(db *sql.DB) (ServerType, error) {
	var varName, versionComment string
	err := db.QueryRow("SHOW VARIABLES LIKE 'version_comment'").Scan(&varName, &versionComment)
	if err != nil {
		// If we can't determine the server type, assume MySQL
		return ServerMySQL, nil
	}

	if strings.Contains(strings.ToLower(versionComment), "oceanbase") {
		return ServerOceanBase, nil
	}
	return ServerMySQL, nil
}
