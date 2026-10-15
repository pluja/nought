// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"path/filepath"
)

// Paths lists every location nought reads or writes.
type Paths struct {
	ConfigFile string `json:"config_file"`
	DataDir    string `json:"data_dir"`
	ArchiveDir string `json:"archive_dir"`
	IndexFile  string `json:"index_file"`
	TagsDump   string `json:"tags_dump"`
	Journal    string `json:"journal"`
	LockFile   string `json:"lock_file"`
	// Transient holds deleted and expiring mail and its tags. Keep it out
	// of backups: it is mail the user wants gone.
	TransientDir  string `json:"transient_dir"`
	TransientDump string `json:"transient_dump"`
}

// TransientArchive returns the Maildir for an account's deleted and
// expiring mail.
func (p Paths) TransientArchive(account string) string {
	return filepath.Join(p.TransientDir, account)
}

// AccountArchive returns the Maildir directory of an account. Account names
// are validated to be safe path components (see validName).
func (p Paths) AccountArchive(account string) string {
	return filepath.Join(p.ArchiveDir, account)
}

// BackupSet returns the paths that must be backed up, the ones that may be
// skipped because they can be rebuilt, and the ones that must never be
// backed up (deleted and expiring mail).
func (p Paths) BackupSet() (include, optional, never []string) {
	return []string{p.ArchiveDir, p.TagsDump, p.Journal, p.ConfigFile},
		[]string{p.IndexFile},
		[]string{p.TransientDir}
}

const appName = "nought"

// ResolvePaths computes the platform paths. NOUGHT_HOME, when set, puts both
// config and data under one directory, which is handy for tests and portable
// setups. getenv is injected so tests never depend on the real environment.
func ResolvePaths(goos string, getenv func(string) string) (Paths, error) {
	configDir, dataDir, err := baseDirs(goos, getenv)
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		ConfigFile: filepath.Join(configDir, "config.toml"),
		DataDir:    dataDir,
		ArchiveDir: filepath.Join(dataDir, "archive"),
		IndexFile:  filepath.Join(dataDir, "index.db"),
		TagsDump:   filepath.Join(dataDir, "tags.dump"),
		Journal:    filepath.Join(dataDir, "expunge.journal"),
		LockFile:   filepath.Join(dataDir, "sync.lock"),

		TransientDir:  filepath.Join(dataDir, "transient"),
		TransientDump: filepath.Join(dataDir, "transient", "tags.dump"),
	}, nil
}

func baseDirs(goos string, getenv func(string) string) (configDir, dataDir string, err error) {
	if home := getenv("NOUGHT_HOME"); home != "" {
		if !filepath.IsAbs(home) {
			return "", "", errors.New("NOUGHT_HOME must be an absolute path")
		}
		return filepath.Join(home, "config"), filepath.Join(home, "data"), nil
	}

	switch goos {
	case "windows":
		roaming, local := getenv("AppData"), getenv("LocalAppData")
		if roaming == "" || local == "" {
			return "", "", errors.New("AppData and LocalAppData must be set")
		}
		return filepath.Join(roaming, appName), filepath.Join(local, appName), nil
	case "darwin", "ios":
		home := getenv("HOME")
		if home == "" {
			return "", "", errors.New("HOME is not set")
		}
		support := filepath.Join(home, "Library", "Application Support", appName)
		return support, support, nil
	default:
		home := getenv("HOME")
		configBase := xdgDir(getenv("XDG_CONFIG_HOME"), home, ".config")
		dataBase := xdgDir(getenv("XDG_DATA_HOME"), home, filepath.Join(".local", "share"))
		if configBase == "" || dataBase == "" {
			return "", "", errors.New("HOME is not set")
		}
		return filepath.Join(configBase, appName), filepath.Join(dataBase, appName), nil
	}
}

// xdgDir follows the XDG base directory spec: relative values are ignored.
func xdgDir(value, home, fallback string) string {
	if value != "" && filepath.IsAbs(value) {
		return value
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, fallback)
}
