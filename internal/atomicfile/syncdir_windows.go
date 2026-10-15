// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package atomicfile

import "os"

// Windows cannot fsync a directory handle; NTFS journals renames itself.
func syncDir(*os.File) error { return nil }
