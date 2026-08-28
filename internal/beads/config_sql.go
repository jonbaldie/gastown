package beads

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"

	_ "github.com/go-sql-driver/mysql"
)

// EnsureDoltConfigValue writes a config key directly to the configured Dolt
// database. Fresh setup uses this to avoid older bd config/schema bootstrap paths.
func EnsureDoltConfigValue(beadsDir, key, value string) error {
	if beadsDir == "" {
		return fmt.Errorf("empty beads directory")
	}
	database := DatabaseNameFromMetadata(beadsDir)
	if database == "" {
		return fmt.Errorf("missing dolt_database in %s", beadsDir)
	}
	meta := readDoltMetadata(beadsDir)
	host := doltConfigHost(meta.Host)
	port := doltConfigPort(meta.Port)
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("invalid Dolt port %q: %w", port, err)
	}

	dsn := fmt.Sprintf("root@tcp(%s)/%s?parseTime=true", net.JoinHostPort(host, port), url.PathEscape(database))
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("REPLACE INTO config (`key`, `value`) VALUES (?, ?)", key, value)
	return err
}

func doltConfigHost(metadataHost string) string {
	return firstNonEmpty(metadataHost, os.Getenv("GT_DOLT_HOST"), "127.0.0.1")
}

func doltConfigPort(metadataPort string) string {
	return firstNonEmpty(
		metadataPort,
		os.Getenv("BEADS_DOLT_SERVER_PORT"),
		os.Getenv("BEADS_DOLT_PORT"),
		os.Getenv("GT_DOLT_PORT"),
		"3307",
	)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
