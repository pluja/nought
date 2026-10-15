// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package store

import "os"

// infoSep is ";" on Windows, where ":" is not allowed in file names.
const infoSep = ";"

// Windows cannot fsync a directory handle; NTFS journals renames itself.
func syncDir(*os.File) error { return nil }
