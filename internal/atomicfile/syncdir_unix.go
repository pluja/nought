// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package atomicfile

import "os"

func syncDir(d *os.File) error { return d.Sync() }
